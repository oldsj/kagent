package driver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

const steerTaskID = "task-1"

func TestSDKDriverSteering(t *testing.T) {
	t.Run("Given a running turn, when a steer arrives before its result, assert that the turn ends at the result that consumes the steer", func(t *testing.T) {
		f := newSteerFixture(t, func(s *steerScript, b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
			if s.prompt == "" {
				s.prompt = op.UUID
				b.SDK(sdkLine(t, `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"working. "}]}}`))
				return
			}
			// The SDK finishes the prompt's run, then runs the queued steer.
			b.SDK(sdkLine(t, `{"type":"result","subtype":"success","result":"working.","user_message_uuids":["`+s.prompt+`"]}`))
			b.SDK(sdkLine(t, `{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"steered: `+op.Text+`"}]}}`))
			b.SDK(sdkLine(t, `{"type":"result","subtype":"success","result":"steered","user_message_uuids":["`+op.UUID+`"]}`))
		})
		f.WhenTurnRuns()
		f.WhenSteered("m-steer", "use main")

		outcome, err := f.AwaitOutcome()

		if err != nil || outcome.Failure != nil || outcome.Pending != nil {
			t.Fatalf("outcome = %+v err = %v, want completed", outcome, err)
		}
		if got := f.sink.text.String(); got != "working. |steered: use main" {
			t.Fatalf("text = %q, want both runs in the turn, split where the steer's run starts", got)
		}
		if len(f.sink.unconsumed) != 0 {
			t.Fatalf("unconsumed = %v, want none for a completed turn", f.sink.unconsumed)
		}
	})

	t.Run("Given a running turn, when the same steer arrives twice, assert that the bridge receives it once", func(t *testing.T) {
		f := newSteerFixture(t, holdPrompt)
		f.WhenTurnRuns()
		f.WhenSteered("m-steer", "once")
		f.WhenSteered("m-steer", "once")

		if got := f.userOps(); len(got) != 2 {
			t.Fatalf("user ops = %v, want the prompt and one steer", got)
		}
		f.cancel()
		_, _ = f.AwaitOutcome()
	})

	t.Run("Given no running turn, when a steer arrives, assert that it is refused as not running", func(t *testing.T) {
		f := newSteerFixture(t, holdPrompt)

		err := f.driver.Steer(context.Background(), runtime.Steer{TaskID: steerTaskID, MessageID: "m-steer", Text: "x"})

		if !errors.Is(err, runtime.SteerTaskNotRunning) {
			t.Fatalf("err = %v, want %v", err, runtime.SteerTaskNotRunning)
		}
	})

	t.Run("Given a running turn, when a steer names another task, assert that it is refused as not running", func(t *testing.T) {
		f := newSteerFixture(t, holdPrompt)
		f.WhenTurnRuns()

		err := f.driver.Steer(context.Background(), runtime.Steer{TaskID: "other-task", MessageID: "m-steer", Text: "x"})

		if !errors.Is(err, runtime.SteerTaskNotRunning) {
			t.Fatalf("err = %v, want %v", err, runtime.SteerTaskNotRunning)
		}
		f.cancel()
		_, _ = f.AwaitOutcome()
	})

	t.Run("Given a turn waiting on a permission prompt, when a steer arrives, assert that it is refused as input required", func(t *testing.T) {
		f := newSteerFixture(t, func(s *steerScript, b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "Bash", "input": map[string]any{}, "toolUseID": "call1"})
		})
		f.WhenTurnRuns()
		outcome, err := f.AwaitOutcome()
		if err != nil || outcome.Pending == nil {
			t.Fatalf("outcome = %+v err = %v, want a live permission prompt", outcome, err)
		}

		err = f.driver.Steer(context.Background(), runtime.Steer{TaskID: steerTaskID, MessageID: "m-steer", Text: "x"})

		if !errors.Is(err, runtime.SteerInputRequired) {
			t.Fatalf("err = %v, want %v", err, runtime.SteerInputRequired)
		}
		_ = outcome.Pending.Cancel(context.Background(), f.sink)
	})

	t.Run("Given a turn that owns a queued steer, when the bridge exits, assert that the steer is reported unconsumed", func(t *testing.T) {
		f := newSteerFixture(t, func(s *steerScript, b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
			if s.prompt == "" {
				s.prompt = op.UUID
				return
			}
			go b.Exit()
		})
		f.WhenTurnRuns()
		f.WhenSteered("m-steer", "lost")

		_, err := f.AwaitOutcome()

		if !errors.Is(err, errRunnerExited) {
			t.Fatalf("err = %v, want the runner exit", err)
		}
		if !slices.Equal(f.sink.unconsumed, []string{"m-steer"}) {
			t.Fatalf("unconsumed = %v, want [m-steer]", f.sink.unconsumed)
		}
	})

	t.Run("Given a turn that owns a queued steer, when Claude defers a protected call, assert that the turn parks and reports the steer unconsumed", func(t *testing.T) {
		actor := newDurableActor(t, DeferredTool{ID: "call-1", Name: "mcp__protected__deploy", Input: map[string]any{"env": "prod"}})
		actor.claude.gate = make(chan struct{})
		sink := &steerSink{}
		outcomes := make(chan steerResult, 1)
		go func() {
			outcome, err := actor.driver.Run(t.Context(), runtime.Turn{TaskID: steerTaskID, ContextID: "context-1", MessageID: "m-prompt", Prompt: "deploy"}, sink)
			outcomes <- steerResult{outcome, err}
		}()
		require.Eventually(t, func() bool { return actor.claude.userOps() == 1 }, 5*time.Second, 10*time.Millisecond)
		awaitSteerable(t, actor.driver)
		require.NoError(t, actor.driver.Steer(t.Context(), runtime.Steer{TaskID: steerTaskID, MessageID: "m-steer", Text: "use main"}))
		require.Eventually(t, func() bool { return actor.claude.userOps() == 2 }, 5*time.Second, 10*time.Millisecond)

		close(actor.claude.gate)

		result := <-outcomes
		require.NoError(t, result.err)
		require.NotNil(t, result.outcome.Pending, "the turn parks on the deferred call")
		require.Equal(t, []string{"m-steer"}, sink.unconsumed)
		err := actor.driver.Steer(t.Context(), runtime.Steer{TaskID: steerTaskID, MessageID: "m-late", Text: "x"})
		require.ErrorIs(t, err, runtime.SteerTaskNotRunning)
		// The cleanup replay resumes a transcript that holds the call without
		// a result, so Claude runs its hook again and takes the stop decision.
		actor.claude.dangling = true
		require.NoError(t, result.outcome.Pending.Cancel(t.Context(), sink))
	})

	t.Run("Given a turn that owns a queued steer, when it is canceled, assert that it interrupts until the steer is consumed", func(t *testing.T) {
		f := newSteerFixture(t, func(s *steerScript, b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
			if s.prompt == "" {
				s.prompt = op.UUID
				return
			}
			s.queued = append(s.queued, op.UUID)
		})
		f.script.onInterrupt = func(s *steerScript, b *sdkrunnertest.Bridge) {
			// Each interrupt ends the run in progress; the next queued message
			// then starts its own run.
			next := s.prompt
			if s.interrupts > 1 {
				next, s.queued = s.queued[0], s.queued[1:]
			}
			b.SDK(sdkLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"interrupted","user_message_uuids":["`+next+`"]}`))
		}
		f.WhenTurnRuns()
		f.WhenSteered("m-steer", "queued")
		f.cancel()

		_, err := f.AwaitOutcome()

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want canceled", err)
		}
		if got := f.script.Interrupts(); got != 2 {
			t.Fatalf("interrupts = %d, want one per owned run", got)
		}
		if len(f.sink.unconsumed) != 0 {
			t.Fatalf("unconsumed = %v, want none once every run was interrupted", f.sink.unconsumed)
		}
		err = f.driver.Steer(context.Background(), runtime.Steer{TaskID: steerTaskID, MessageID: "m-late", Text: "x"})
		if !errors.Is(err, runtime.SteerTaskNotRunning) {
			t.Fatalf("late steer err = %v, want %v", err, runtime.SteerTaskNotRunning)
		}
	})
}

// holdPrompt never finishes the prompt, so the turn stays running.
func holdPrompt(s *steerScript, _ *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
	if s.prompt == "" {
		s.prompt = op.UUID
	}
}

type steerSink struct {
	recordingSink
	unconsumed []string
}

func (s *steerSink) SteersUnconsumed(ids []string) { s.unconsumed = append(s.unconsumed, ids...) }

// SteerRunStarted marks the run boundary in the recorded text.
func (s *steerSink) SteerRunStarted() { s.text.WriteString("|") }

// steerScript scripts the fake bridge. Its handlers run on the bridge's op
// reader, one op at a time, so they must not block.
type steerScript struct {
	mu          sync.Mutex
	prompt      string
	queued      []string
	interrupts  int
	onUser      func(*steerScript, *sdkrunnertest.Bridge, sdkrunnertest.Op)
	onInterrupt func(*steerScript, *sdkrunnertest.Bridge)
	users       []string
	submitted   chan struct{}
}

func (s *steerScript) handle(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op.Op {
	case "user":
		s.users = append(s.users, op.Text)
		s.onUser(s, b, op)
		s.submitted <- struct{}{}
	case "interrupt":
		s.interrupts++
		if s.onInterrupt != nil {
			s.onInterrupt(s, b)
		}
		b.Emit(map[string]any{"k": "ack", "ref": op.Ref, "what": op.Op, "ms": 1})
	case "model", "effort", "mode":
		b.Emit(map[string]any{"k": "ack", "ref": op.Ref, "what": op.Op, "ms": 1})
	}
}

func (s *steerScript) Interrupts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts
}

type steerFixture struct {
	t        *testing.T
	driver   *SDKDriver
	script   *steerScript
	sink     *steerSink
	ctx      context.Context
	cancel   context.CancelFunc
	outcomes chan steerResult
}

type steerResult struct {
	outcome runtime.Outcome
	err     error
}

func newSteerFixture(t *testing.T, onUser func(*steerScript, *sdkrunnertest.Bridge, sdkrunnertest.Op)) *steerFixture {
	t.Helper()
	script := &steerScript{onUser: onUser, submitted: make(chan struct{}, 16)}
	driver, _, _ := newSDKFixture(t, script.handle)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &steerFixture{t: t, driver: driver, script: script, sink: &steerSink{}, ctx: ctx, cancel: cancel, outcomes: make(chan steerResult, 1)}
}

// WhenTurnRuns starts the A2A turn and waits until the bridge has its prompt
// and the turn accepts steers.
func (f *steerFixture) WhenTurnRuns() {
	f.t.Helper()
	go func() {
		outcome, err := f.driver.Run(f.ctx, runtime.Turn{TaskID: steerTaskID, MessageID: "m-prompt", Prompt: "start"}, f.sink)
		f.outcomes <- steerResult{outcome, err}
	}()
	f.awaitSubmitted()
	awaitSteerable(f.t, f.driver)
}

// awaitSteerable waits until d's running turn accepts steers. Claude can see a
// turn's prompt before SubmitMessage returns to Run, which opens steering only
// after that.
func awaitSteerable(t *testing.T, d *SDKDriver) {
	t.Helper()
	require.Eventually(t, func() bool {
		d.steerMu.Lock()
		defer d.steerMu.Unlock()
		return d.steering != nil
	}, 5*time.Second, time.Millisecond, "the turn never accepted steers")
}

// WhenSteered steers the turn; a new message is awaited at the bridge.
func (f *steerFixture) WhenSteered(messageID, text string) {
	f.t.Helper()
	before := len(f.userOps())
	if err := f.driver.Steer(context.Background(), runtime.Steer{TaskID: steerTaskID, MessageID: messageID, Text: text}); err != nil {
		f.t.Fatalf("Steer() error = %v", err)
	}
	if len(f.userOps()) == before {
		select {
		case <-f.script.submitted:
		case <-time.After(100 * time.Millisecond):
			// A repeated steer sends nothing.
		}
	}
}

func (f *steerFixture) awaitSubmitted() {
	f.t.Helper()
	select {
	case <-f.script.submitted:
	case <-time.After(5 * time.Second):
		f.t.Fatal("the bridge never received the prompt")
	}
}

func (f *steerFixture) userOps() []string {
	f.script.mu.Lock()
	defer f.script.mu.Unlock()
	return slices.Clone(f.script.users)
}

func (f *steerFixture) AwaitOutcome() (runtime.Outcome, error) {
	f.t.Helper()
	select {
	case result := <-f.outcomes:
		return result.outcome, result.err
	case <-time.After(5 * time.Second):
		f.t.Fatal("the turn did not end")
		return runtime.Outcome{}, nil
	}
}
