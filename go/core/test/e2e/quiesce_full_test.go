package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/mockllm"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/metadata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

const (
	quiesceStartToolID = "toolu_kagent_quiesce_start"
	quiesceCheckToolID = "toolu_kagent_quiesce_check"
	// The pid file is durable, so the check distinguishes a resumed process
	// from a cold start that kept only files.
	quiesceStartCommand = `setsid sleep 86400 </dev/null >/dev/null 2>&1 & echo $! > /data/workspace/.quiesce-pid; ` +
		`sleep 1; kill -0 "$(cat /data/workspace/.quiesce-pid)" && echo PROCESS_STARTED`
	quiesceCheckCommand = `kill -0 "$(cat /data/workspace/.quiesce-pid)" 2>/dev/null && echo PROCESS_ALIVE || echo PROCESS_GONE`
)

// TestE2EClaudeFullQuiesceKeepsProcesses runs a Claude Harness with
// snapshotPolicy.onQuiesce Full. A detached process started in the first turn
// must still be running after the idle Actor suspends at MEMORY fidelity and
// the next turn resumes it.
func TestE2EClaudeFullQuiesceKeepsProcesses(t *testing.T) {
	t.Parallel()
	target := interactionTarget(t)
	server := mockllm.NewServer(quiesceFullMock(t))
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
		// mockllm matches only the last message; drop Claude's trailing
		// system-role context message as claude_turn_test does.
		if body, err := io.ReadAll(r.Body); err == nil {
			body = withoutTrailingSystemMessages(body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			r.Header.Del("Content-Length")
		}
		proxy.ServeHTTP(w, r)
	}), nil)
	kube := interactionKubeClient(t)
	harness := createFullQuiesceHarness(t, kube)
	model := createClaudeMockModelNamed(t, kube, claudeTurnConfiguredModel, strings.TrimSuffix(reachableModelURL(t, recorder.URL), "/v1"))
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "quiesce-full-", Namespace: "kagent", Labels: testHarness{name: harness.Name, runtimeLabel: "claude"}.labels()},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig:  &v1alpha3.ObjectReference{Name: model.Name},
			Description:  "Full quiesce E2E fixture",
			SystemPrompt: "Reply briefly.",
		},
	}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harness.Name)
	fixture := newInteractionFixtureForHarnessTemplate(t, target, harness.Name, template.Name)

	task := sendClaudeTurn(t, fixture, "kagent-quiesce-proof start", "", "")
	if task.Status.State != a2atype.TaskStateCompleted || !strings.Contains(taskText(task), "QUIESCE_STARTED") {
		t.Fatalf("start task state = %s, text = %q", task.Status.State, taskText(task))
	}
	if output := recordedToolResult(recorder, quiesceStartToolID); !strings.Contains(output, "PROCESS_STARTED") {
		t.Fatalf("start command output = %q, want PROCESS_STARTED", output)
	}

	waitActorSuspendedAt(t, fixture, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)

	task = sendClaudeTurn(t, fixture, "kagent-quiesce-proof check", "", "")
	if task.Status.State != a2atype.TaskStateCompleted || !strings.Contains(taskText(task), "QUIESCE_CHECKED") {
		t.Fatalf("check task state = %s, text = %q", task.Status.State, taskText(task))
	}
	if output := recordedToolResult(recorder, quiesceCheckToolID); !strings.Contains(output, "PROCESS_ALIVE") {
		t.Fatalf("check command output = %q, want PROCESS_ALIVE after a MEMORY resume", output)
	}
}

// createFullQuiesceHarness clones the suite's Claude Harness with onQuiesce Full.
func createFullQuiesceHarness(t *testing.T, kube ctrlclient.Client) *v1alpha3.Harness {
	t.Helper()
	base := &v1alpha3.Harness{}
	if err := kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: claudeE2EHarness}, base); err != nil {
		t.Fatalf("get Claude Harness: %v", err)
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "quiesce-full-", Namespace: "kagent"},
		Spec:       *base.Spec.DeepCopy(),
	}
	harness.Spec.Substrate.SnapshotPolicy.OnQuiesce = v1alpha3.RuntimeSnapshotScopeFull
	if err := kube.Create(t.Context(), harness); err != nil {
		t.Fatalf("create Full quiesce Harness: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), harness); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Full quiesce Harness: %v", err)
		}
	})
	return harness
}

// waitActorSuspendedAt waits for the Session's Actor to suspend with a durable
// snapshot of the given fidelity.
func waitActorSuspendedAt(t *testing.T, fixture *interactionFixture, fidelity ateapipb.SnapshotFidelity) {
	t.Helper()
	actorID := substrate.ActorName(fixture.sessionID)
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(t.Context(), "x-user-id", "e2e"), time.Minute)
	defer cancel()
	var observed ateapipb.SnapshotFidelity
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		actor, err := findSubstrateActor(ctx, fixture.system, "", actorID)
		if err != nil {
			return false, err
		}
		if actor == nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
			return false, nil
		}
		snapshot := substrate.LatestActorSnapshot(actor)
		observed = snapshot.GetFidelity()
		return substrate.SnapshotURI(snapshot) != "" && observed == fidelity, nil
	})
	if err != nil {
		t.Fatalf("Actor %s did not suspend with a durable %s snapshot (last %s): %v", actorID, fidelity, observed, err)
	}
}

// recordedToolResult returns the text of the tool result with toolID from the
// model requests the recorder saw.
func recordedToolResult(recorder *modelRecorder, toolID string) string {
	recorder.mu.Lock()
	recorded := append([]recordedModelRequest(nil), recorder.requests...)
	recorder.mu.Unlock()
	for _, request := range recorded {
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(request.Body, &body) != nil {
			continue
		}
		for _, message := range body.Messages {
			var blocks []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(message.Content, &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				if block.Type == "tool_result" && block.ToolUseID == toolID {
					return toolResultText(block.Content)
				}
			}
		}
	}
	return ""
}

func quiesceFullMock(t *testing.T) mockllm.Config {
	t.Helper()
	textMatch := func(text string) map[string]any {
		return map[string]any{"match_type": "contains", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	resultMatch := func(toolID string) map[string]any {
		return map[string]any{"match_type": "contains", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolID},
		}}}
	}
	reply := func(id string, content map[string]any) map[string]any {
		stop := "end_turn"
		if content["type"] == "tool_use" {
			stop = "tool_use"
		}
		return map[string]any{"id": id, "type": "message", "role": "assistant", "content": []any{content}, "model": claudeTurnConfiguredModel, "stop_reason": stop, "usage": map[string]int{"input_tokens": 10, "output_tokens": 4}}
	}
	bash := func(id, command string) map[string]any {
		return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"command": command}}
	}
	text := func(value string) map[string]any { return map[string]any{"type": "text", "text": value} }
	mocks := []map[string]any{
		{"name": "start", "match": textMatch("kagent-quiesce-proof start"), "response": reply("msg_quiesce_start", bash(quiesceStartToolID, quiesceStartCommand))},
		{"name": "started", "match": resultMatch(quiesceStartToolID), "response": reply("msg_quiesce_started", text("QUIESCE_STARTED"))},
		{"name": "check", "match": textMatch("kagent-quiesce-proof check"), "response": reply("msg_quiesce_check", bash(quiesceCheckToolID, quiesceCheckCommand))},
		{"name": "checked", "match": resultMatch(quiesceCheckToolID), "response": reply("msg_quiesce_checked", text("QUIESCE_CHECKED"))},
		// Claude's auxiliary requests, such as title generation, match last.
		{"name": "auxiliary", "match": textMatch(""), "response": reply("msg_auxiliary", text("ok"))},
	}
	raw, err := json.Marshal(map[string]any{"anthropic": mocks})
	if err != nil {
		t.Fatal(err)
	}
	var cfg mockllm.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode Full quiesce mock: %v", err)
	}
	return cfg
}
