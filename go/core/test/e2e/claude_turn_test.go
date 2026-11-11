package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/kagent-dev/mockllm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
)

const (
	claudeTurnConfiguredModel = "claude-sonnet-5-5"
	claudeTurnOverrideModel   = "claude-opus-5-5"
	claudeTurnProbeToolID     = "toolu_kagent_runtime_probe"
	// claudeTurnProbe runs as the harness's own Bash tool. Substrate creates
	// the durable directory root-owned with mode 0777; the harness must run as
	// its image's non-root user and still own what it writes there.
	claudeTurnProbe = `id -u; id -g; stat -c '%u:%g %a %n' /data /data/workspace /data/claude /data/adapter; ` +
		`printf ok > /data/workspace/.kagent-runtime-probe && stat -c 'probe %u:%g %a' /data/workspace/.kagent-runtime-probe && echo PROBE_WRITE_OK`
)

// TestE2EClaudeTurnOverridesAndRuntimeUser sends three turns through the
// public A2A API and checks what reaches the model: a turn with
// kagent.dev/model and kagent.dev/effort uses them; the next turn, with neither
// key, is back on the configured model and its default effort; an effort-only
// turn keeps the configured model. The first turn also runs a Bash probe
// showing the harness runs non-root and can write the durable workspace.
func TestE2EClaudeTurnOverridesAndRuntimeUser(t *testing.T) {
	t.Parallel()
	target := interactionTarget(t)
	server := mockllm.NewServer(claudeTurnMock(t))
	baseURL, err := server.Start(t.Context())
	if err != nil {
		t.Fatalf("start mock LLM: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Errorf("stop mock LLM: %v", err)
		}
	})
	proxy := mockLLMProxy(t, baseURL)
	recorder := startModelRecorder(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Claude 5 family requests end with a system-role context message.
		// mockllm matches only the last message, so it sees the request
		// without it; the recorder keeps the original.
		if body, err := io.ReadAll(r.Body); err == nil {
			body = withoutTrailingSystemMessages(body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			r.Header.Del("Content-Length")
		}
		proxy.ServeHTTP(w, r)
	}), nil)
	kube := interactionKubeClient(t)
	model := createClaudeMockModelNamed(t, kube, claudeTurnConfiguredModel, strings.TrimSuffix(reachableModelURL(t, recorder.URL), "/v1"))
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "claude-turn-", Namespace: "kagent", Labels: testHarness{name: claudeE2EHarness, runtimeLabel: "claude"}.labels()},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig:  &v1alpha3.ObjectReference{Name: model.Name},
			Description:  "Claude per-turn override E2E fixture",
			SystemPrompt: "Reply briefly.",
		},
	}
	createAndWaitInteractionTemplateForHarness(t, kube, template, claudeE2EHarness)
	fixture := newInteractionFixtureForHarnessTemplate(t, target, claudeE2EHarness, template.Name)

	for _, turn := range []struct {
		marker, model, effort string
	}{
		{"turn-one", claudeTurnOverrideModel, "high"},
		{"turn-two", "", ""},
		{"turn-three", "", "low"},
	} {
		task := sendClaudeTurn(t, fixture, "kagent-turn-proof "+turn.marker, turn.model, turn.effort)
		if task.Status.State != a2atype.TaskStateCompleted || !strings.Contains(taskText(task), "TURN_DONE "+turn.marker) {
			t.Fatalf("%s task state = %s, text = %q", turn.marker, task.Status.State, taskText(task))
		}
	}

	turns, probe := claudeTurnRequests(t, recorder)
	t.Logf("runtime probe output:\n%s", probe)
	for _, marker := range []string{"turn-one", "turn-two", "turn-three"} {
		t.Logf("%s agent-loop requests (model, effort): %v", marker, turns[marker])
	}
	for _, want := range []struct {
		marker, model, effort string
	}{
		{"turn-one", claudeTurnOverrideModel, "high"},
		{"turn-three", claudeTurnConfiguredModel, "low"},
	} {
		requests := turns[want.marker]
		if len(requests) == 0 {
			t.Fatalf("no agent-loop model request recorded for %s", want.marker)
		}
		for _, request := range requests {
			if request.model != want.model || request.effort != want.effort {
				t.Errorf("%s model request = model %q, effort %q; want %q, %q", want.marker, request.model, request.effort, want.model, want.effort)
			}
		}
	}
	// An absent key means the default for that turn, never the previous
	// turn's value. The CLI may send the model's own default effort.
	if len(turns["turn-two"]) == 0 {
		t.Fatal("no agent-loop model request recorded for turn-two")
	}
	for _, request := range turns["turn-two"] {
		if request.model != claudeTurnConfiguredModel || request.effort == "high" || request.effort == "low" {
			t.Errorf("turn-two model request = model %q, effort %q; want the configured model and default effort", request.model, request.effort)
		}
	}

	lines := strings.Split(strings.TrimSpace(probe), "\n")
	if len(lines) < 2 || lines[0] == "0" || lines[1] == "0" {
		t.Errorf("harness Bash tool ran as uid/gid %q, want non-root", lines)
	}
	if !strings.Contains(probe, "PROBE_WRITE_OK") || !strings.Contains(probe, "probe "+lines[0]+":") {
		t.Errorf("harness could not write the durable workspace as its own user:\n%s", probe)
	}
}

func sendClaudeTurn(t *testing.T, fixture *interactionFixture, text, model, effort string) *a2atype.Task {
	t.Helper()
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	message.ContextID = fixture.sessionID
	if model != "" || effort != "" {
		message.Metadata = map[string]any{}
	}
	if model != "" {
		message.Metadata[kagenta2a.ModelMetadataKey] = model
	}
	if effort != "" {
		message.Metadata[kagenta2a.EffortMetadataKey] = effort
	}
	request, err := pbconv.ToProtoSendMessageRequest(&a2atype.SendMessageRequest{Tenant: fixture.tenant, Message: message})
	if err != nil {
		t.Fatalf("build A2A request: %v", err)
	}
	response, err := sendMessageWithRetry(fixture.ctx, fixture.client, request)
	if err != nil {
		t.Fatalf("send A2A message: %v", err)
	}
	result, err := pbconv.FromProtoSendMessageResponse(response)
	if err != nil {
		t.Fatalf("decode A2A response: %v", err)
	}
	task, ok := result.(*a2atype.Task)
	if !ok {
		t.Fatalf("A2A response = %T, want Task", result)
	}
	return task
}

type claudeTurnRequest struct{ model, effort string }

// claudeTurnRequests groups the recorded agent-loop requests (those offering
// tools) by the turn marker in their last user message, and returns the
// probe's tool result text. Auxiliary requests such as title generation are
// not part of the turn and are ignored.
func claudeTurnRequests(t *testing.T, recorder *modelRecorder) (map[string][]claudeTurnRequest, string) {
	t.Helper()
	recorder.mu.Lock()
	recorded := append([]recordedModelRequest(nil), recorder.requests...)
	recorder.mu.Unlock()
	turns := map[string][]claudeTurnRequest{}
	var probe string
	for _, request := range recorded {
		var body struct {
			Model        string            `json:"model"`
			Tools        []json.RawMessage `json:"tools"`
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(withoutTrailingSystemMessages(request.Body), &body); err != nil || len(body.Tools) == 0 || len(body.Messages) == 0 {
			continue
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "user" {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(last.Content, &blocks); err != nil {
			var text string
			if json.Unmarshal(last.Content, &text) != nil {
				continue
			}
			blocks = append(blocks, struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			}{Type: "text", Text: text})
		}
		for _, block := range blocks {
			marker := ""
			switch {
			case block.Type == "tool_result" && block.ToolUseID == claudeTurnProbeToolID:
				marker, probe = "turn-one", toolResultText(block.Content)
			case block.Type == "text":
				for _, candidate := range []string{"turn-one", "turn-two", "turn-three"} {
					if strings.Contains(block.Text, "kagent-turn-proof "+candidate) {
						marker = candidate
					}
				}
			}
			if marker != "" {
				turns[marker] = append(turns[marker], claudeTurnRequest{model: body.Model, effort: body.OutputConfig.Effort})
				break
			}
		}
	}
	return turns, probe
}

func toolResultText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &parts)
	var joined []string
	for _, part := range parts {
		joined = append(joined, part.Text)
	}
	return strings.Join(joined, "\n")
}

func claudeTurnMock(t *testing.T) mockllm.Config {
	t.Helper()
	textMatch := func(text string) map[string]any {
		return map[string]any{"match_type": "contains", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	reply := func(id string, content ...map[string]any) map[string]any {
		stop := "end_turn"
		if content[0]["type"] == "tool_use" {
			stop = "tool_use"
		}
		return map[string]any{"id": id, "type": "message", "role": "assistant", "content": content, "model": claudeTurnConfiguredModel, "stop_reason": stop, "usage": map[string]int{"input_tokens": 10, "output_tokens": 4}}
	}
	text := func(value string) map[string]any { return map[string]any{"type": "text", "text": value} }
	mocks := []map[string]any{
		{"name": "turn_one_probe", "match": textMatch("kagent-turn-proof turn-one"), "response": reply("msg_turn_one_probe", map[string]any{
			"type": "tool_use", "id": claudeTurnProbeToolID, "name": "Bash", "input": map[string]string{"command": claudeTurnProbe},
		})},
		{"name": "turn_one_done", "match": map[string]any{"match_type": "contains", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": claudeTurnProbeToolID},
		}}}, "response": reply("msg_turn_one_done", text("TURN_DONE turn-one"))},
	}
	for _, marker := range []string{"turn-two", "turn-three"} {
		mocks = append(mocks, map[string]any{"name": marker, "match": textMatch("kagent-turn-proof " + marker), "response": reply("msg_"+marker, text("TURN_DONE "+marker))})
	}
	// Claude's auxiliary requests, such as title generation, match last.
	mocks = append(mocks, map[string]any{"name": "auxiliary", "match": textMatch(""), "response": reply("msg_auxiliary", text("ok"))})
	raw, err := json.Marshal(map[string]any{"anthropic": mocks})
	if err != nil {
		t.Fatal(err)
	}
	var cfg mockllm.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode Claude turn mock: %v", err)
	}
	return cfg
}

func withoutTrailingSystemMessages(body []byte) []byte {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	messages, _ := request["messages"].([]any)
	trimmed := len(messages)
	for trimmed > 0 {
		message, _ := messages[trimmed-1].(map[string]any)
		if message["role"] != "system" {
			break
		}
		trimmed--
	}
	if trimmed == len(messages) {
		return body
	}
	request["messages"] = messages[:trimmed]
	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}
