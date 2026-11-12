package driver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// A tool call inside the approval boundary must stay answerable after the Actor
// is suspended to a DATA snapshot, which keeps /data and no process.
func TestSDKApprovalSurvivesARestart(t *testing.T) {
	tool := DeferredTool{ID: "call-1", Name: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}}

	t.Run("Given a protected call parked by an A2A turn, when the Actor restarts and the call is approved, assert that the same call runs once and the turn completes", func(t *testing.T) {
		actor := newDurableActor(t, tool)
		request := actor.GivenTurnParkedOn(t)
		require.Equal(t, &runtime.ApprovalRequest{
			ID: request.(*runtime.ApprovalRequest).ID, CallID: "call-1", Name: "mcp__protected__deploy",
			Args: map[string]any{"env": "prod"}, Hint: "Claude requires approval before calling mcp__protected__deploy.",
		}, request)

		restarted := actor.WhenActorRestarts(t)
		sink := &recordingSink{}
		outcome, err := restarted.pending.Resume(t.Context(), &runtime.ApprovalDecision{ID: request.(*runtime.ApprovalRequest).ID, Approved: true}, sink)

		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
		require.Nil(t, outcome.Pending)
		require.Equal(t, "deployed", sink.text.String())
		require.Len(t, sink.results, 1)
		require.Equal(t, "call-1", sink.results[0].ID)
		require.Equal(t, "mcp__protected__deploy", sink.results[0].Name, "the result is attributed to the call the first turn reported")
		restarted.AssertThatConversationWasResumedWithoutAPrompt(t)
		answer := restarted.claude.permissionAnswer(t)
		require.Equal(t, "allow", answer.Behavior)
		require.Equal(t, map[string]any{"env": "prod"}, answer.UpdatedInput)
		require.Equal(t, phaseComplete, restarted.phase(t))
	})

	t.Run("Given a protected call parked by an A2A turn, when the Actor restarts and the call is denied, assert that Claude receives the reason and the call does not run", func(t *testing.T) {
		actor := newDurableActor(t, tool)
		request := actor.GivenTurnParkedOn(t).(*runtime.ApprovalRequest)

		restarted := actor.WhenActorRestarts(t)
		sink := &recordingSink{}
		outcome, err := restarted.pending.Resume(t.Context(), &runtime.ApprovalDecision{ID: request.ID, RejectionReason: "production is serving traffic"}, sink)

		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
		require.Equal(t, "refused: production is serving traffic", sink.text.String())
		restarted.AssertThatConversationWasResumedWithoutAPrompt(t)
		require.Empty(t, restarted.claude.permissionAnswers(), "a refused call never reaches the permission prompt")
		require.Equal(t, phaseComplete, restarted.phase(t))
	})

	t.Run("Given a protected call parked by an A2A turn, when the Actor restarts and the task is canceled, assert that the call is refused, Claude is interrupted and nothing stays pending", func(t *testing.T) {
		actor := newDurableActor(t, tool)
		actor.GivenTurnParkedOn(t)

		restarted := actor.WhenActorRestarts(t)
		err := restarted.pending.Cancel(t.Context(), &recordingSink{})

		require.NoError(t, err)
		require.Equal(t, phaseCanceled, restarted.phase(t))
		require.Eventually(t, func() bool { return restarted.claude.sawOp("interrupt") }, 5*time.Second, 5*time.Millisecond)
		_, _, pending, err := restarted.driver.RestorePending()
		require.NoError(t, err)
		require.Nil(t, pending)
	})

	t.Run("Given an approved call that has already run, when the task is canceled before Claude answers it, assert that Claude is interrupted and the task ends canceled", func(t *testing.T) {
		actor := newDurableActor(t, tool)
		request := actor.GivenTurnParkedOn(t).(*runtime.ApprovalRequest)
		restarted := actor.WhenActorRestarts(t)
		restarted.claude.holdAnswer = true
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sink := &resultSignalSink{ran: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			_, err := restarted.pending.Resume(ctx, &runtime.ApprovalDecision{ID: request.ID, Approved: true}, sink)
			done <- err
		}()
		select {
		case <-sink.ran:
		case <-time.After(5 * time.Second):
			t.Fatal("the approved call did not run")
		}

		cancel()

		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("the canceled replay did not settle")
		}
		require.True(t, restarted.claude.sawOp("interrupt"))
		require.Equal(t, phaseCanceled, restarted.phase(t))
	})
}

// resultSignalSink reports when the replayed call's result arrives.
type resultSignalSink struct {
	recordingSink
	ran  chan struct{}
	once sync.Once
}

func (s *resultSignalSink) ToolResult(event runtime.ToolResult) error {
	s.once.Do(func() { close(s.ran) })
	return s.recordingSink.ToolResult(event)
}

func TestSDKQuestionSurvivesARestart(t *testing.T) {
	t.Run("Given a question parked by an A2A turn, when the Actor restarts and the user answers, assert that Claude receives the answer with the question", func(t *testing.T) {
		questions := []any{map[string]any{
			"question": "Which database?", "header": "DB", "multiSelect": false,
			"options": []any{map[string]any{"label": "PostgreSQL", "description": "pg"}, map[string]any{"label": "SQLite", "description": "lite"}},
		}}
		actor := newDurableActor(t, DeferredTool{ID: "ask-1", Name: "AskUserQuestion", Input: map[string]any{"questions": questions}})
		request, ok := actor.GivenTurnParkedOn(t).(*runtime.AskUserRequest)
		require.True(t, ok, "the parked request is a question, not a tool approval")
		require.Len(t, request.Questions, 1)
		require.Equal(t, "Which database?", request.Questions[0].Question)
		require.Equal(t, []string{"PostgreSQL", "SQLite"}, request.Questions[0].Choices)

		restarted := actor.WhenActorRestarts(t)
		outcome, err := restarted.pending.Resume(t.Context(), &runtime.AskUserResponse{ID: request.ID, Answers: [][]string{{"PostgreSQL"}}}, &recordingSink{})

		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
		answer := restarted.claude.permissionAnswer(t)
		require.Equal(t, "allow", answer.Behavior)
		require.Equal(t, map[string]any{"Which database?": "PostgreSQL"}, answer.UpdatedInput["answers"])
		require.Equal(t, questions, answer.UpdatedInput["questions"])
	})
}

func TestSDKPromptsOutsideTheJournal(t *testing.T) {
	t.Run("Given an A2A turn, when a protected call reaches the prompt without having been deferred, assert that it is refused so Claude retries it alone", func(t *testing.T) {
		actor := newDurableActor(t, DeferredTool{ID: "call-1", Name: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}})
		actor.claude.skipHook = true

		outcome, err := actor.driver.Run(t.Context(), runtime.Turn{TaskID: "task-1", ContextID: "context-1", Prompt: "deploy"}, &recordingSink{})

		require.NoError(t, err)
		require.Nil(t, outcome.Pending, "a protected call is never held as a live prompt")
		answer := actor.claude.permissionAnswer(t)
		require.Equal(t, "deny", answer.Behavior)
		require.Contains(t, answer.Message, "retry it as a single tool call")
	})

	t.Run("Given no A2A turn, when a client's own turn calls a protected tool, assert that the hook leaves it to the live prompt", func(t *testing.T) {
		actor := newDurableActor(t, DeferredTool{ID: "call-1", Name: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}})
		input := hookInput{SessionID: sdkSessionID, Event: "PreToolUse", ToolID: "call-1", ToolName: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}}

		withoutTurn := postHook(t, actor.broker, input)
		sub, err := actor.session.BeginTurn()
		require.NoError(t, err)
		withTurn := postHook(t, actor.broker, input)
		actor.session.EndTurn(sub)

		require.Empty(t, withoutTurn.Specific.Decision, "nothing could park a call deferred here")
		require.Equal(t, "defer", withTurn.Specific.Decision)
	})

	t.Run("Given an approved call replayed after a restart, when Claude then raises a prompt outside the approval boundary, assert that the journal is closed and the live prompt still resolves the turn", func(t *testing.T) {
		actor := newDurableActor(t, DeferredTool{ID: "call-1", Name: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}})
		request := actor.GivenTurnParkedOn(t).(*runtime.ApprovalRequest)
		restarted := actor.WhenActorRestarts(t)
		restarted.claude.thenPrompt = "Bash"
		sink := &recordingSink{}

		outcome, err := restarted.pending.Resume(t.Context(), &runtime.ApprovalDecision{ID: request.ID, Approved: true}, sink)

		require.NoError(t, err)
		require.NotNil(t, outcome.Pending, "the turn waits on the live prompt")
		live := outcome.Pending.Request().(*runtime.ApprovalRequest)
		require.Equal(t, "Bash", live.Name)
		require.Equal(t, phaseComplete, restarted.phase(t), "the replayed call is settled")
		_, _, restored, err := restarted.driver.RestorePending()
		require.NoError(t, err)
		require.Nil(t, restored, "a later turn must not find an abandoned replay")
		final, err := outcome.Pending.Resume(t.Context(), &runtime.ApprovalDecision{ID: live.ID, Approved: true}, sink)
		require.NoError(t, err)
		require.Nil(t, final.Pending)
		require.Equal(t, "deployed", sink.text.String())
		require.False(t, restarted.session.TurnActive())
	})
}

// durableActor is one Actor lifetime: a driver, its bridge and approval broker
// over a durable directory that outlives it.
type durableActor struct {
	dir          string
	tool         DeferredTool
	continuation *sdkrunnertest.Continuation
	broker       *ApprovalBroker
	session      *sdkrunner.Session
	driver       *SDKDriver
	launcher     *sdkrunnertest.Launcher
	claude       *fakeClaude
	pending      runtime.PendingTurn
}

func newDurableActor(t *testing.T, tool DeferredTool) *durableActor {
	t.Helper()
	return startDurableActor(t, t.TempDir(), tool, &sdkrunnertest.Continuation{}, false)
}

func startDurableActor(t *testing.T, dir string, tool DeferredTool, continuation *sdkrunnertest.Continuation, dangling bool) *durableActor {
	t.Helper()
	broker, err := NewApprovalBroker([]string{"protected"}, filepath.Join(dir, "adapter"), 1<<20)
	require.NoError(t, err)
	claude := &fakeClaude{t: t, broker: broker, tool: tool, dangling: dangling}
	launcher := sdkrunnertest.NewLauncher(claude.handle)
	launcher.OnStart = claude.started
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: launcher,
		Spec: func(sdkrunner.Profile) (sdkrunner.Spec, error) {
			return sdkrunner.Spec{Cwd: "/w", Executable: "claude"}, nil
		},
		Continuation: continuation, Dir: filepath.Join(dir, "sdk-runner"), StopGrace: time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		_ = broker.Close()
	})
	base := NewProcessDriver(ProcessConfig{ApprovalBroker: broker, MaxEventBytes: 1 << 20, InterruptGrace: 200 * time.Millisecond, TurnTimeout: time.Minute})
	return &durableActor{
		dir: dir, tool: tool, continuation: continuation, broker: broker, session: session,
		driver: NewSDKDriver(base, session), launcher: launcher, claude: claude,
	}
}

// GivenTurnParkedOn runs an A2A turn until Claude defers the tool call, and
// returns the request the task publishes.
func (a *durableActor) GivenTurnParkedOn(t *testing.T) runtime.InputRequest {
	t.Helper()
	sink := &recordingSink{}
	outcome, err := a.driver.Run(t.Context(), runtime.Turn{TaskID: "task-1", ContextID: "context-1", Prompt: "deploy"}, sink)
	require.NoError(t, err)
	require.NotNil(t, outcome.Pending, "the turn parks on the deferred call")
	require.Len(t, sink.calls, 1)
	require.Equal(t, a.tool.ID, sink.calls[0].ID)
	require.Equal(t, phaseWaiting, a.phase(t))
	require.False(t, a.session.TurnActive(), "a parked call holds no live turn")
	require.False(t, a.session.Busy(), "nothing keeps the Actor awake while the call waits")
	require.Empty(t, a.claude.permissionAnswers(), "the call was deferred, not prompted")
	return outcome.Pending.Request()
}

// WhenActorRestarts discards every process and in-memory handle, as a DATA
// snapshot does, and recovers the parked turn from the durable directory.
func (a *durableActor) WhenActorRestarts(t *testing.T) *durableActor {
	t.Helper()
	require.NoError(t, a.session.Close())
	require.NoError(t, a.broker.Close())
	restarted := startDurableActor(t, a.dir, a.tool, a.continuation, true)
	taskID, contextID, pending, err := restarted.driver.RestorePending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the parked turn is recovered from disk")
	require.Equal(t, "task-1", taskID)
	require.Equal(t, "context-1", contextID)
	restarted.pending = pending
	return restarted
}

func (a *durableActor) AssertThatConversationWasResumedWithoutAPrompt(t *testing.T) {
	t.Helper()
	require.Equal(t, 1, a.launcher.Launches())
	bridge := a.launcher.Started(time.Second)
	require.NotNil(t, bridge)
	require.Equal(t, sdkSessionID, bridge.Spec.Resume)
	require.False(t, a.claude.sawOp("user"), "a new message would make Claude drop the deferred call")
}

func (a *durableActor) phase(t *testing.T) string {
	t.Helper()
	record, err := loadDeferred(filepath.Join(a.dir, "adapter"), 1<<20)
	require.NoError(t, err)
	require.NotNil(t, record)
	return record.Phase
}

// fakeClaude scripts the bridge the way Claude Code behaves around a deferred
// tool call under the Agent SDK. It asks the real approval broker's hook, as
// Claude does. Observed with SDK 0.3.285:
//   - a "defer" decision ends the turn with a tool_deferred result that names
//     the turn's user message, and the bridge stays up;
//   - a resumed conversation runs the hook for the same call at once, with no
//     user message, and its result names no user message;
//   - after "allow", the ask rule still sends the call to the permission prompt
//     with the input Claude originally asked about;
//   - after "deny", the reason is the tool result and Claude answers it.
type fakeClaude struct {
	t      *testing.T
	broker *ApprovalBroker
	tool   DeferredTool
	// dangling means the transcript holds the call without a result.
	dangling bool
	// skipHook sends the call straight to the prompt, as Claude does for a
	// call it cannot defer.
	skipHook bool
	// holdAnswer keeps Claude working after the call ran, until interrupted.
	holdAnswer bool
	// thenPrompt is a tool outside the approval boundary that Claude asks about
	// once the deferred call has run.
	thenPrompt string
	// gate holds the call until it is closed, so a steer can arrive while the
	// turn runs. Later user messages then only queue, as Claude queues them.
	gate chan struct{}

	mu      sync.Mutex
	ops     []sdkrunnertest.Op
	perms   []sdkrunnertest.Op
	replied []string
}

func (c *fakeClaude) started(b *sdkrunnertest.Bridge) {
	if b.Spec.Resume != "" && c.dangling {
		c.call(b, nil)
	}
}

func (c *fakeClaude) handle(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
	c.mu.Lock()
	c.ops = append(c.ops, op)
	c.mu.Unlock()
	switch op.Op {
	case "user":
		if c.gate != nil && c.userOps() > 1 {
			return
		}
		b.SDK(map[string]any{"type": "system", "subtype": "init", "session_id": sdkSessionID})
		b.SDK(map[string]any{"type": "assistant", "message": map[string]any{"id": "m1", "content": []any{
			map[string]any{"type": "tool_use", "id": c.tool.ID, "name": c.tool.Name, "input": c.tool.Input},
		}}})
		if c.gate != nil {
			go func() {
				<-c.gate
				c.call(b, []string{op.UUID})
			}()
			return
		}
		c.call(b, []string{op.UUID})
	case "perm":
		c.mu.Lock()
		c.perms = append(c.perms, op)
		replied := c.replied
		c.mu.Unlock()
		if op.Behavior == "allow" && c.holdAnswer {
			c.toolResult(b, "ok", false)
		} else if op.Behavior == "allow" && c.thenPrompt != "" && op.ID == "perm-1" {
			c.toolResult(b, "ok", false)
			b.Emit(map[string]any{"k": "perm", "id": "perm-2", "toolName": c.thenPrompt, "input": map[string]any{"command": "ls"}, "toolUseID": "call-2"})
		} else if op.Behavior == "allow" && op.ID == "perm-2" {
			b.SDK(map[string]any{"type": "assistant", "message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": "deployed"}}}})
			b.SDK(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "deployed", "session_id": sdkSessionID})
		} else if op.Behavior == "allow" {
			c.finish(b, "ok", false, "deployed", replied)
		} else {
			c.finish(b, op.Message, true, "refused: "+op.Message, replied)
		}
	case "interrupt":
		b.Emit(map[string]any{"k": "ack", "ref": op.Ref, "what": "interrupt", "ms": 1})
		if c.holdAnswer {
			b.SDK(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "interrupted", "session_id": sdkSessionID})
		}
	}
}

// call runs the tool call through the hook. uuids are the user messages the
// turn's result will name.
func (c *fakeClaude) call(b *sdkrunnertest.Bridge, uuids []string) {
	c.mu.Lock()
	c.replied = uuids
	c.mu.Unlock()
	decision, reason := "", ""
	if !c.skipHook {
		output := postHook(c.t, c.broker, hookInput{SessionID: sdkSessionID, Event: "PreToolUse", ToolID: c.tool.ID, ToolName: c.tool.Name, Input: c.tool.Input})
		decision, reason = output.Specific.Decision, output.Specific.Reason
	}
	switch decision {
	case "defer":
		result := map[string]any{
			"type": "result", "subtype": "success", "is_error": false, "stop_reason": "tool_deferred", "result": "",
			"session_id": sdkSessionID, "deferred_tool_use": map[string]any{"id": c.tool.ID, "name": c.tool.Name, "input": c.tool.Input},
		}
		if len(uuids) != 0 {
			result["user_message_uuids"] = uuids
		}
		b.SDK(result)
	case "deny":
		c.finish(b, "PreToolUse:"+c.tool.Name+" hook error: "+reason, true, "refused: "+reason, uuids)
	default:
		// Allowed, or left undecided: the ask rule prompts with the original input.
		b.Emit(map[string]any{"k": "perm", "id": "perm-1", "toolName": c.tool.Name, "input": c.tool.Input, "toolUseID": c.tool.ID})
	}
}

func (c *fakeClaude) toolResult(b *sdkrunnertest.Bridge, content string, isError bool) {
	b.SDK(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": c.tool.ID, "content": content, "is_error": isError},
	}}})
}

func (c *fakeClaude) finish(b *sdkrunnertest.Bridge, toolResult string, isError bool, text string, uuids []string) {
	c.toolResult(b, toolResult, isError)
	b.SDK(map[string]any{"type": "assistant", "message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": text}}}})
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": sdkSessionID}
	if len(uuids) != 0 {
		result["user_message_uuids"] = uuids
	}
	b.SDK(result)
}

func (c *fakeClaude) permissionAnswers() []sdkrunnertest.Op {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.perms)
}

func (c *fakeClaude) permissionAnswer(t *testing.T) sdkrunnertest.Op {
	t.Helper()
	answers := c.permissionAnswers()
	require.Len(t, answers, 1, "the prompt is answered exactly once")
	// Round-trip so numbers compare as the bridge would read them.
	raw, err := json.Marshal(answers[0])
	require.NoError(t, err)
	var answer sdkrunnertest.Op
	require.NoError(t, json.Unmarshal(raw, &answer))
	return answer
}

func (c *fakeClaude) userOps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, op := range c.ops {
		if op.Op == "user" {
			n++
		}
	}
	return n
}

func (c *fakeClaude) sawOp(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.ops, func(op sdkrunnertest.Op) bool { return op.Op == name })
}
