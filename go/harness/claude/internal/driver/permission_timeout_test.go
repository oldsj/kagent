package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakePermissionResponse struct {
	result *mcp.CallToolResult
	err    error
}

// This client models the native idle floor and overall limit with a manually advanced
// clock. It invokes the real broker handler in memory, without a socket, model,
// or actual suspension. Adapter tests separately pin the generated policy.
type fakePermissionCall struct {
	elapsed  time.Duration
	limit    time.Duration
	cancel   context.CancelFunc
	response <-chan fakePermissionResponse
}

func newFakePermissionCall(t *testing.T, broker *ApprovalBroker, nativeJSON []byte, callID string) *fakePermissionCall {
	t.Helper()
	var native struct {
		Servers map[string]struct {
			Timeout *int64 `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(nativeJSON, &native); err != nil {
		t.Fatal(err)
	}
	limit := 5 * time.Minute
	if timeout := native.Servers["kagent_hitl"].Timeout; timeout != nil {
		// The override floors the idle window and is also the hard overall
		// bound, so the call's first effective deadline is the override.
		limit = time.Duration(*timeout) * time.Millisecond
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	response := make(chan fakePermissionResponse, 1)
	go func() {
		result, _, err := broker.handle(ctx, nil, &permissionPromptInput{
			ToolName: "mcp__protected__write", ToolUseID: callID,
			Input: map[string]any{"value": float64(7)},
		})
		response <- fakePermissionResponse{result: result, err: err}
	}()
	return &fakePermissionCall{limit: limit, cancel: cancel, response: response}
}

func (c *fakePermissionCall) advance(elapsed time.Duration) {
	c.elapsed += elapsed
	if c.elapsed >= c.limit {
		c.cancel()
	}
}

func fakeApprovalBroker() *ApprovalBroker {
	return &ApprovalBroker{
		protected: map[string]struct{}{"protected": {}},
		requests:  make(chan *PendingApprovalRequest, 2), active: make(map[string]struct{}),
	}
}

func permissionPolicyJSON(t *testing.T, timeout *int64) []byte {
	t.Helper()
	cfg := config.Production("claude-test", "help")
	cfg.MCPServers = map[string]config.MCPServer{"kagent_hitl": {
		Type: "http", URL: "http://127.0.0.1/mcp", TimeoutMillis: timeout,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := cfg.MCPConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPermissionClientModelsIdleExpiry(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout *int64
		elapsed time.Duration
	}{
		{name: "native default expires during observed pause", elapsed: 2378 * time.Second},
		{name: "short override retains its overall bound", timeout: new(int64(1000)), elapsed: time.Second},
		{name: "finite approval bound expires", timeout: new(int64(100_000_000)), elapsed: 100_000_000 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker := fakeApprovalBroker()
			call := newFakePermissionCall(t, broker, permissionPolicyJSON(t, test.timeout), "call-old")
			pending := <-broker.Requests()
			call.advance(test.elapsed)
			response := <-call.response
			if !errors.Is(response.err, context.Canceled) {
				t.Fatalf("expired permission response = %#v", response)
			}
			if pending.waiting() {
				t.Fatal("expired permission call is still waiting")
			}
			if err := pending.resolve(runtime.ApprovalDecision{ID: pending.request.ID, Approved: true}); err == nil {
				t.Fatal("expired call accepted a late decision")
			}
		})
	}
}

func TestProcessDriverPermissionSurvivesSuspendedWallTime(t *testing.T) {
	for _, action := range []string{"approve", "reject", "cancel"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			decisionFile := filepath.Join(dir, "decision")
			invocations := filepath.Join(dir, "invocations")
			prompts := filepath.Join(dir, "prompts")
			executable := filepath.Join(dir, "claude")
			script := `#!/bin/sh
cat > "$PROMPTS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}'
while [ ! -f "$DECISION" ]; do sleep 0.01; done
if [ "$(cat "$DECISION")" = allow ]; then printf '%s\n' 'call-original' >> "$INVOCATIONS"; fi
printf '%s\n' '{"type":"result","subtype":"success","session_id":"11111111-1111-4111-8111-111111111111"}'
`
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			broker := fakeApprovalBroker()
			call := newFakePermissionCall(t, broker, permissionPolicyJSON(t, new(int64(100_000_000))), "call-original")
			bridged := bridgePermissionToFile(t, call, decisionFile)
			driver := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir,
				Environment:   []string{"DECISION=" + decisionFile, "INVOCATIONS=" + invocations, "PROMPTS=" + prompts},
				MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 100 * time.Millisecond,
				ApprovalBroker: broker,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			sink := &recordingSink{}
			outcome, err := driver.Run(ctx, runtime.Turn{Prompt: "write once"}, sink)
			if err != nil || outcome.Pending == nil {
				t.Fatalf("Run() = %#v, %v", outcome, err)
			}
			parked := outcome.Pending.(*pendingTurn)
			t.Cleanup(func() { _ = parked.Cancel(context.Background()) })
			session, process, sessionID := parked.session, parked.session.command.Process, parked.session.sessionID
			request := parked.Request().(*runtime.ApprovalRequest)
			if request.ID == "" || request.CallID != "call-original" {
				t.Fatalf("original approval = %#v", request)
			}
			call.advance(2378 * time.Second)
			if call.elapsed >= call.limit || !parked.pending.waiting() || process.Signal(syscall.Signal(0)) != nil {
				t.Fatal("original call/process did not survive simulated inactivity")
			}
			if action == "cancel" {
				err = parked.Cancel(ctx)
			} else {
				outcome, err = parked.Resume(ctx, &runtime.ApprovalDecision{
					ID: request.ID, Approved: action == "approve", RejectionReason: "operator denied",
				}, sink)
				if outcome.Pending != nil || outcome.Failure != nil {
					t.Fatalf("Resume() = %#v", outcome)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			<-bridged
			if parked.session != session || parked.session.command.Process != process || parked.session.sessionID != sessionID {
				t.Fatal("approval replaced the original process/session")
			}
			if len(sink.sessions) != 1 {
				t.Fatalf("session starts = %#v", sink.sessions)
			}
			gotPrompts, err := os.ReadFile(prompts)
			if err != nil {
				t.Fatal(err)
			}
			wantPrompt, err := userMessage("write once")
			if err != nil {
				t.Fatal(err)
			}
			if string(gotPrompts) != string(wantPrompt) {
				t.Fatalf("prompts = %s, want one original prompt", gotPrompts)
			}
			calls, err := os.ReadFile(invocations)
			if action == "approve" {
				if err != nil || string(calls) != "call-original\n" {
					t.Fatalf("protected invocations = %q, %v", calls, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("denied/canceled call invoked protected endpoint: %q, %v", calls, err)
			}
			decision, err := os.ReadFile(decisionFile)
			if err != nil {
				t.Fatal(err)
			}
			want := "deny"
			if action == "approve" {
				want = "allow"
			}
			if string(decision) != want {
				t.Fatalf("native decision = %q, want %q", decision, want)
			}
		})
	}
}

func bridgePermissionToFile(t *testing.T, call *fakePermissionCall, path string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		response := <-call.response
		if response.err != nil {
			// Test cleanup may cancel an unanswered call.
			if !errors.Is(response.err, context.Canceled) {
				t.Errorf("permission call: %v", response.err)
			}
			return
		}
		if len(response.result.Content) != 1 {
			t.Error("permission response must contain one decision")
			return
		}
		content, ok := response.result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Error("permission response must contain text")
			return
		}
		var output permissionPromptOutput
		if err := json.Unmarshal([]byte(content.Text), &output); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, []byte(output.Behavior), 0o600); err != nil {
			t.Error(err)
		}
	}()
	return done
}

type approvalActivitySink struct {
	recordingSink
	calls   []runtime.ToolCall
	results []runtime.ToolResult
	onCall  func(runtime.ToolCall)
}

func (s *approvalActivitySink) ToolCall(call runtime.ToolCall) error {
	s.calls = append(s.calls, call)
	if s.onCall != nil {
		s.onCall(call)
	}
	return nil
}

func (s *approvalActivitySink) ToolResult(result runtime.ToolResult) error {
	s.results = append(s.results, result)
	return nil
}

func TestAcceptedApprovalDoesNotAuthorizeRetryAfterIdleError(t *testing.T) {
	dir := t.TempDir()
	fixture, err := filepath.Abs("../../testdata/stream-permission-idle-retry.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	firstDecision, secondDecision := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	executable := filepath.Join(dir, "claude")
	script := `#!/bin/sh
cat > "$PROMPTS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}'
while [ ! -f "$FIRST" ]; do sleep 0.01; done
cat "$FIXTURE"
while [ ! -f "$SECOND" ]; do sleep 0.01; done
printf '%s\n' '{"type":"result","subtype":"success","session_id":"11111111-1111-4111-8111-111111111111"}'
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	broker := fakeApprovalBroker()
	policy := permissionPolicyJSON(t, nil)
	oldCall := newFakePermissionCall(t, broker, policy, "call-old")
	oldBridge := bridgePermissionToFile(t, oldCall, firstDecision)
	var newCall *fakePermissionCall
	var newBridge <-chan struct{}
	sink := &approvalActivitySink{onCall: func(call runtime.ToolCall) {
		if call.ID == "call-new" {
			newCall = newFakePermissionCall(t, broker, policy, "call-new")
			newBridge = bridgePermissionToFile(t, newCall, secondDecision)
			t.Cleanup(func() {
				newCall.cancel()
				<-newBridge
			})
		}
	}}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir,
		Environment:   []string{"FIRST=" + firstDecision, "SECOND=" + secondDecision, "FIXTURE=" + fixture, "PROMPTS=" + filepath.Join(dir, "prompts")},
		MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 100 * time.Millisecond, ApprovalBroker: broker,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := driver.Run(ctx, runtime.Turn{Prompt: "write"}, sink)
	if err != nil || outcome.Pending == nil {
		t.Fatalf("Run() = %#v, %v", outcome, err)
	}
	original := outcome.Pending.(*pendingTurn)
	t.Cleanup(func() { _ = original.Cancel(context.Background()) })
	oldRequest := original.Request().(*runtime.ApprovalRequest)
	// The fixture models the native timer winning after the bridge accepts the
	// exact original decision. Transport acceptance is not tool execution.
	outcome, err = original.Resume(ctx, &runtime.ApprovalDecision{ID: oldRequest.ID, Approved: true}, sink)
	if err != nil || outcome.Pending == nil {
		t.Fatalf("Resume() = %#v, %v", outcome, err)
	}
	<-oldBridge
	retry := outcome.Pending.(*pendingTurn)
	newRequest := retry.Request().(*runtime.ApprovalRequest)
	if newRequest.ID == oldRequest.ID || newRequest.CallID != "call-new" || newRequest.Name != oldRequest.Name || !reflect.DeepEqual(newRequest.Args, oldRequest.Args) {
		t.Fatalf("retry approval = %#v, old = %#v", newRequest, oldRequest)
	}
	if retry.session != original.session {
		t.Fatal("retry started a new native process")
	}
	if len(sink.results) != 1 || sink.results[0].ID != "call-old" || !sink.results[0].IsError || !strings.Contains(sink.results[0].Result.(string), "2378s") {
		t.Fatalf("original idle error was not preserved: %#v", sink.results)
	}
	if len(sink.calls) != 2 || sink.calls[0].ID != "call-old" || sink.calls[1].ID != "call-new" || !reflect.DeepEqual(sink.calls[0].Arguments, sink.calls[1].Arguments) {
		t.Fatalf("observed native calls = %#v", sink.calls)
	}
	if _, err := os.Stat(secondDecision); !os.IsNotExist(err) {
		t.Fatalf("retry inherited an approval: %v", err)
	}
	if _, err := retry.Resume(ctx, &runtime.ApprovalDecision{ID: oldRequest.ID, Approved: true}, sink); err == nil || !strings.Contains(err.Error(), "does not match pending ID") {
		t.Fatalf("stale approval Resume() error = %v", err)
	}
	if len(retry.pending.decision) != 0 {
		t.Fatal("stale approval reached the new call")
	}
	// Cancel the in-memory fake client independently of the now-reaped process.
	newCall.cancel()
	<-newBridge
}
