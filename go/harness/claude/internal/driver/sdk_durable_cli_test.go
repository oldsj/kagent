package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// These names select a real Claude Code executable and a real Agent SDK bridge
// for a test that checks deferral and replay offline against a fake model API.
// The scripted bridge in sdk_durable_test.go encodes how Claude behaves; this
// test checks that encoding against the pinned release.
//
// Point claudeCLIEnvName at the pinned CLI (CLAUDE_CODE_VERSION in the harness
// Dockerfile) and sdkRunnerEnvName at a runner.mjs whose directory has the
// pinned SDK installed: /opt/kagent/sdk-runner/runner.mjs in the harness
// image, or sdk-runner/runner.mjs after `npm ci` there. Run as a non-root
// user. The test is skipped when either is unset.
const (
	claudeCLIEnvName = "KAGENT_TEST_CLAUDE_CLI"
	sdkRunnerEnvName = "KAGENT_TEST_CLAUDE_SDK_RUNNER"
)

func TestRealBridgeAnswersADeferredQuestionAfterARestart(t *testing.T) {
	cli, runner := os.Getenv(claudeCLIEnvName), os.Getenv(sdkRunnerEnvName)
	if cli == "" || runner == "" {
		t.Skipf("%s and %s are not both set", claudeCLIEnvName, sdkRunnerEnvName)
	}
	model := newQuestionModel(t)
	actor := newRealBridgeActor(t, cli, runner, model.server.URL, t.TempDir(), &sdkrunnertest.Continuation{})

	// Given a turn in which Claude asks the user a question.
	outcome, err := actor.driver.Run(t.Context(), runtime.Turn{TaskID: "task-1", ContextID: "context-1", Prompt: "Which database should we use?"}, &recordingSink{})
	require.NoError(t, err)
	require.NotNil(t, outcome.Pending, "the turn parks on the deferred question")
	request, ok := outcome.Pending.Request().(*runtime.AskUserRequest)
	require.True(t, ok, "request = %#v, want a question", outcome.Pending.Request())
	require.Equal(t, "Which database?", request.Questions[0].Question)
	require.False(t, actor.session.Busy(), "nothing keeps the Actor awake while the question waits")

	// When every process is gone, as after a DATA snapshot, and the user answers.
	require.NoError(t, actor.session.Close())
	require.NoError(t, actor.broker.Close())
	restarted := newRealBridgeActor(t, cli, runner, model.server.URL, actor.dir, actor.continuation)
	taskID, contextID, pending, err := restarted.driver.RestorePending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, "task-1", taskID)
	require.Equal(t, "context-1", contextID)
	sink := &recordingSink{}
	final, err := pending.Resume(t.Context(), &runtime.AskUserResponse{ID: request.ID, Answers: [][]string{{"PostgreSQL"}}}, sink)

	// Assert that Claude received the answer and finished the turn.
	require.NoError(t, err)
	require.Nil(t, final.Failure)
	require.Nil(t, final.Pending)
	require.Equal(t, "Using PostgreSQL.", sink.text.String())
	require.Contains(t, model.answered(), "PostgreSQL", "the model sees the user's answer as the tool result")
	require.Equal(t, 1, model.userPrompts(), "the replay sent no second user message")
	record, err := loadDeferred(filepath.Join(actor.dir, "adapter"), 1<<20)
	require.NoError(t, err)
	require.Equal(t, phaseComplete, record.Phase)
}

type realBridgeActor struct {
	dir          string
	continuation *sdkrunnertest.Continuation
	broker       *ApprovalBroker
	session      *sdkrunner.Session
	driver       *SDKDriver
}

func newRealBridgeActor(t *testing.T, cli, runner, modelURL, dir string, continuation *sdkrunnertest.Continuation) *realBridgeActor {
	t.Helper()
	workspace := filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	broker, err := NewApprovalBroker(nil, filepath.Join(dir, "adapter"), 1<<20)
	require.NoError(t, err)
	environment := []string{
		// The bridge needs node, and the CLI its tools.
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"),
		"ANTHROPIC_BASE_URL=" + modelURL,
		"ANTHROPIC_API_KEY=test-key",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	base := NewProcessDriver(ProcessConfig{
		Executable: cli, Workspace: workspace, Model: "claude-sonnet-4-5", Environment: environment,
		ApprovalBroker: broker, MaxEventBytes: 1 << 20, InterruptGrace: 5 * time.Second, TurnTimeout: 2 * time.Minute,
	})
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: sdkrunner.NodeLauncher{Node: "node", Script: runner, Dir: filepath.Dir(runner), Env: environment},
		Spec: func(profile sdkrunner.Profile) (sdkrunner.Spec, error) {
			return base.SDKSpec(profile, filepath.Join(dir, "sdk-runner", "home"))
		},
		Continuation: continuation, Dir: filepath.Join(dir, "sdk-runner"), StopGrace: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		_ = broker.Close()
	})
	return &realBridgeActor{dir: dir, continuation: continuation, broker: broker, session: session, driver: NewSDKDriver(base, session)}
}

// questionModel is a minimal Anthropic Messages API. An agent-loop request
// gets one AskUserQuestion call, then a text reply once the conversation holds
// that call's result. Every other request, such as title generation, gets a
// short text reply.
type questionModel struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []string
}

func newQuestionModel(t *testing.T) *questionModel {
	t.Helper()
	model := &questionModel{}
	model.server = httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(model.server.Close)
	return model
}

// answered is the agent-loop request that carried the question's result.
func (m *questionModel) answered() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, request := range m.requests {
		if strings.Contains(request, `"tool_result"`) {
			return request
		}
	}
	return ""
}

// userPrompts counts the distinct user prompts the agent loop was sent.
func (m *questionModel) userPrompts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := ""
	for _, request := range m.requests {
		last = request
	}
	return strings.Count(last, "Which database should we use?")
}

func (m *questionModel) serve(response http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	if strings.HasSuffix(request.URL.Path, "/count_tokens") {
		_, _ = response.Write([]byte(`{"input_tokens":1}`))
		return
	}
	if !strings.HasSuffix(request.URL.Path, "/v1/messages") {
		http.NotFound(response, request)
		return
	}
	var decoded struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &decoded)
	agentLoop := false
	for _, tool := range decoded.Tools {
		agentLoop = agentLoop || tool.Name == "AskUserQuestion"
	}
	block, stop := map[string]any{"type": "text", "text": "ok"}, "end_turn"
	if agentLoop {
		m.mu.Lock()
		m.requests = append(m.requests, string(body))
		m.mu.Unlock()
		if strings.Contains(string(body), `"tool_result"`) {
			block = map[string]any{"type": "text", "text": "Using PostgreSQL."}
		} else {
			block, stop = map[string]any{"type": "tool_use", "id": "toolu_question", "name": "AskUserQuestion", "input": map[string]any{
				"questions": []any{map[string]any{
					"question": "Which database?", "header": "Database", "multiSelect": false,
					"options": []any{
						map[string]any{"label": "PostgreSQL", "description": "Relational"},
						map[string]any{"label": "SQLite", "description": "Embedded"},
					},
				}},
			}}, "tool_use"
		}
	}
	message := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-test-model",
		"content": []any{block}, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
	if !decoded.Stream {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(message)
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	startBlock := map[string]any{"type": "text", "text": ""}
	delta := map[string]any{"type": "text_delta", "text": block["text"]}
	if block["type"] == "tool_use" {
		startBlock = map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}
		input, _ := json.Marshal(block["input"])
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(input)}
	}
	start := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-test-model",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
	for _, event := range []map[string]any{
		{"type": "message_start", "message": start},
		{"type": "content_block_start", "index": 0, "content_block": startBlock},
		{"type": "content_block_delta", "index": 0, "delta": delta},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}},
		{"type": "message_stop"},
	} {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}
