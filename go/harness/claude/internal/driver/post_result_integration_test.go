//go:build linux

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func scriptedDriver(t *testing.T, script string, environment ...string) *ProcessDriver {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ncat >/dev/null\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir, Environment: environment,
		MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond,
	})
}

const waitingStream = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success","result":"first"}' '{"type":"result","subtype":"success","result":"last"}' '{"type":"assistant","message":{"id":"msg_wait","content":[{"type":"text","text":"waiting after result"}]}}'
trap '' INT
exec sleep 30
`

type runResult struct {
	outcome runtime.Outcome
	err     error
}

type sdkContextRunner struct {
	driver   *ProcessDriver
	deadline chan bool
	waiting  chan struct{}
	done     chan runResult
}

func (r *sdkContextRunner) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	_, hasDeadline := ctx.Deadline()
	r.deadline <- hasDeadline
	outcome, err := r.driver.Run(ctx, turn, waitingSink{EventSink: sink, waiting: r.waiting})
	r.done <- runResult{outcome: outcome, err: err}
	return outcome, err
}

type waitingSink struct {
	runtime.EventSink
	waiting chan struct{}
}

func (s waitingSink) TextDelta(event runtime.TextDelta) error {
	if event.Text == "waiting after result" {
		close(s.waiting)
	}
	return s.EventSink.TextDelta(event)
}

// The SDK detaches execution from the caller. Exercise its real task store so
// caller disconnection cannot supply the bound or hide a missing terminal event.
func TestSDKDriverOwnedExecutionBounds(t *testing.T) {
	for _, test := range []struct {
		name, script   string
		grace, ceiling time.Duration
		cancelTask     bool
		state          a2atype.TaskState
		failure        string
	}{
		{name: "post-result grace", script: waitingStream, grace: 400 * time.Millisecond, ceiling: 10 * time.Second, state: a2atype.TaskStateCompleted},
		{name: "grace with closed stdout", script: strings.Replace(waitingStream, "exec sleep 30", "exec 1>&-\nexec sleep 30", 1), grace: 400 * time.Millisecond, ceiling: 10 * time.Second, state: a2atype.TaskStateCompleted},
		{name: "grace preserves last failure", script: strings.Replace(waitingStream, `"subtype":"success","result":"last"`, `"subtype":"error_during_execution","is_error":true,"result":"last"`, 1), grace: 400 * time.Millisecond, ceiling: 10 * time.Second, state: a2atype.TaskStateFailed, failure: "last"},
		{name: "overall ceiling after result", script: waitingStream, grace: 10 * time.Second, ceiling: 400 * time.Millisecond, state: a2atype.TaskStateFailed, failure: "Claude execution budget exceeded (approval wait time excluded)"},
		{name: "ceiling with closed stdout", script: strings.Replace(waitingStream, "exec sleep 30", "exec 1>&-\nexec sleep 30", 1), grace: 10 * time.Second, ceiling: 400 * time.Millisecond, state: a2atype.TaskStateFailed, failure: "Claude execution budget exceeded (approval wait time excluded)"},
		{name: "overall ceiling before result", script: strings.ReplaceAll(strings.ReplaceAll(waitingStream, ` '{"type":"result","subtype":"success","result":"first"}'`, ""), ` '{"type":"result","subtype":"success","result":"last"}'`, ""), grace: 10 * time.Second, ceiling: 400 * time.Millisecond, state: a2atype.TaskStateFailed, failure: "Claude execution budget exceeded (approval wait time excluded)"},
		{name: "explicit cancellation", script: waitingStream, grace: 10 * time.Second, ceiling: 10 * time.Second, cancelTask: true, state: a2atype.TaskStateCanceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &sdkContextRunner{
				driver: scriptedDriver(t, test.script), deadline: make(chan bool, 1),
				waiting: make(chan struct{}), done: make(chan runResult, 1),
			}
			runner.driver.config.PostResultGrace = test.grace
			runner.driver.config.TurnTimeout = test.ceiling
			assertSDKTerminalState(t, runner, test.cancelTask, test.state, test.failure)
		})
	}
}

func assertSDKTerminalState(t *testing.T, runner *sdkContextRunner, cancelTask bool, wantState a2atype.TaskState, wantFailure string) {
	t.Helper()
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	store := taskstore.NewInMemory(nil)
	seed := &a2atype.Task{ID: "sdk-bound", ContextID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Status: a2atype.TaskStatus{State: a2atype.TaskStateSubmitted}}
	if _, err := store.Create(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(store))
	callCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
	message.TaskID, message.ContextID = seed.ID, seed.ContextID
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for _, err := range handler.SendStreamingMessage(callCtx, &a2atype.SendMessageRequest{Message: message}) {
			if err != nil {
				return
			}
		}
	}()
	defer func() { _, _ = handler.CancelTask(context.Background(), &a2atype.CancelTaskRequest{ID: seed.ID}) }()
	select {
	case hasDeadline := <-runner.deadline:
		if hasDeadline {
			t.Fatal("SDK unexpectedly propagated a caller deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native runner did not start")
	}
	select {
	case <-runner.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("fake did not emit post-result activity")
	}
	cancel()
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("caller stream did not disconnect")
	}
	if cancelTask {
		if _, err := handler.CancelTask(t.Context(), &a2atype.CancelTaskRequest{ID: seed.ID}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case result := <-runner.done:
		switch {
		case cancelTask:
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancel error = %v", result.err)
			}
		case strings.HasPrefix(wantFailure, "Claude execution budget"):
			if !errors.Is(result.err, errExecutionBudgetExceeded) {
				t.Fatalf("budget error = %v", result.err)
			}
		default:
			if result.err != nil {
				t.Fatalf("grace caused execution error: %v", result.err)
			}
			if wantFailure != "" && (result.outcome.Failure == nil || result.outcome.Failure.Message != wantFailure) {
				t.Fatalf("last outcome = %#v", result.outcome)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detached SDK execution did not finish within the harness bound")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err := handler.GetTask(t.Context(), &a2atype.GetTaskRequest{ID: seed.ID})
		if err != nil {
			t.Fatal(err)
		}
		if task.Status.State.Terminal() {
			if task.Status.State != wantState {
				t.Fatalf("persisted state = %s, want %s", task.Status.State, wantState)
			}
			if wantFailure != "" && (task.Status.Message == nil || task.Status.Message.Parts[0].Text() != wantFailure) {
				t.Fatalf("persisted failure = %#v", task.Status.Message)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task remained %s", task.Status.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPostResultDeadlineReportsA2AFailure(t *testing.T) {
	runner := scriptedDriver(t, strings.Replace(waitingStream, "exec sleep 30", "exec 1>&-\nexec sleep 30", 1))
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
	message.TaskID, message.ContextID = "review-deadline", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
	var terminal int
	for event, err := range executor.Execute(ctx, req) {
		if err != nil {
			t.Fatal(err)
		}
		if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State.Terminal() {
			terminal++
			if status.Status.State != a2atype.TaskStateFailed || status.Status.Message == nil || status.Status.Message.Parts[0].Text() != "Harness execution deadline exceeded" {
				t.Fatalf("deadline terminal event = %#v", status)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal count = %d", terminal)
	}
}

func TestLateNonzeroFailureReportsA2AOnce(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/stream-post-result.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	fixture = []byte(strings.Replace(string(fixture), `"subtype":"success","result":"last"`, `"subtype":"error_during_execution","is_error":true,"result":"last"`, 1))
	path := filepath.Join(t.TempDir(), "stream")
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := scriptedDriver(t, "cat \"$STREAM\"\nexit 1\n", "STREAM="+path)
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
	message.TaskID, message.ContextID = "review-late-error", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
	var terminal int
	for event, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatal(err)
		}
		if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State.Terminal() {
			terminal++
			if status.Status.State != a2atype.TaskStateFailed || status.Status.Message == nil {
				t.Fatalf("terminal event: %#v", status)
			}
			if status.Status.Message.Parts[0].Text() != "last" {
				t.Fatalf("failure message: %#v", status.Status.Message)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal count = %d", terminal)
	}
}

func TestCancelLateApprovalTerminatesDescendants(t *testing.T) {
	activity := filepath.Join(t.TempDir(), "activity")
	runner := scriptedDriver(t, `sh -c 'trap "" INT; while :; do printf x >> "$ACTIVITY"; sleep 0.02; done' >/dev/null 2>&1 &
while [ ! -s "$ACTIVITY" ]; do sleep 0.01; done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}' '{"type":"assistant","message":{"id":"msg_after","content":[{"type":"text","text":"continued after result"}]}}'
trap '' INT
exec sleep 30
`, "ACTIVITY="+activity)
	broker := &ApprovalBroker{requests: make(chan *PendingApprovalRequest, 1)}
	pending := newTestPending("review-late-approval", "review-call")
	runner.config.ApprovalBroker = broker
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	outcome, err := runner.Run(ctx, runtime.Turn{Prompt: "hello"}, &approvalAfterResultSink{broker: broker, pending: pending})
	if err != nil || outcome.Pending == nil {
		t.Fatalf("Run = %#v, %v", outcome, err)
	}
	if err := outcome.Pending.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if decision := <-pending.decision; decision.Approved {
		t.Fatal("Cancel allowed the call")
	}
	before, err := os.Stat(activity)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(activity)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatal("descendant still running after late approval cancel")
	}
}

func TestExecutionBudgetsPauseDuringApproval(t *testing.T) {
	for _, test := range []struct {
		name                   string
		grace, ceiling         time.Duration
		beforeResult, approved bool
		wantBudgetFailure      bool
	}{
		{name: "post-result grace after approval", grace: 200 * time.Millisecond, ceiling: 10 * time.Second, approved: true},
		{name: "overall ceiling after denial", grace: 10 * time.Second, ceiling: 200 * time.Millisecond, wantBudgetFailure: true},
		{name: "overall ceiling before first result", grace: 10 * time.Second, ceiling: 200 * time.Millisecond, beforeResult: true, approved: true, wantBudgetFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := strings.Replace(waitingStream, "waiting after result", "continued after result", 1)
			if test.beforeResult {
				script = strings.ReplaceAll(strings.ReplaceAll(script, ` '{"type":"result","subtype":"success","result":"first"}'`, ""), ` '{"type":"result","subtype":"success","result":"last"}'`, "")
			}
			driver := scriptedDriver(t, script)
			driver.config.PostResultGrace = test.grace
			driver.config.TurnTimeout = test.ceiling
			broker := &ApprovalBroker{requests: make(chan *PendingApprovalRequest, 1)}
			pending := newTestPending("paused-budget", "call")
			driver.config.ApprovalBroker = broker
			sink := &approvalAfterResultSink{broker: broker, pending: pending}
			outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, sink)
			if err != nil || outcome.Pending == nil {
				t.Fatalf("Run = %#v, %v", outcome, err)
			}
			parked := outcome.Pending.(*pendingTurn)
			defer func() { _ = parked.Cancel(t.Context()) }()
			if parked.session.executionBudget.done() != nil || parked.session.postResultBudget.done() != nil {
				t.Fatal("approval left a budget running")
			}
			// Wait longer than both configured active-time limits. Approval wait
			// must consume neither budget, even if the Actor remains running.
			time.Sleep(300 * time.Millisecond)
			if parked.session.executionBudget.expired() || parked.session.postResultBudget.expired() {
				t.Fatal("approval wait exhausted execution time")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			outcome, err = parked.Resume(ctx, &runtime.ApprovalDecision{ID: pending.request.ID, Approved: test.approved}, sink)
			if test.wantBudgetFailure {
				if !errors.Is(err, errExecutionBudgetExceeded) {
					t.Fatalf("Resume budget error = %v", err)
				}
			} else if err != nil || outcome.Failure != nil || outcome.Pending != nil {
				t.Fatalf("Resume grace outcome = %#v, %v", outcome, err)
			}
			if decision := <-pending.decision; decision.Approved != test.approved {
				t.Fatalf("decision = %#v", decision)
			}
		})
	}
}

func TestCompletionCleansDetachedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	activity := filepath.Join(dir, "activity")
	pidPath := filepath.Join(dir, "pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidPath)
		if err != nil {
			t.Errorf("read owned child pid for cleanup: %v", err)
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 1 {
			t.Errorf("invalid owned child pid: %q", raw)
			return
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Errorf("kill owned detached group: %v", err)
		}
	})
	runner := scriptedDriver(t, `setsid /bin/sh -c 'trap "" INT; printf "%s\n" "$$" > "$CHILD_PID"; while :; do printf x >> "$ACTIVITY"; sleep 0.02; done' >/dev/null 2>&1 &
while [ ! -s "$ACTIVITY" ]; do sleep 0.01; done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}' '{"type":"assistant","message":{"id":"msg_after","content":[{"type":"text","text":"after result"}]}}' '{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}'
`, "ACTIVITY="+activity, "CHILD_PID="+pidPath)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	outcome, err := runner.Run(ctx, runtime.Turn{Prompt: "hello"}, &recordingSink{})
	if err != nil || outcome.Failure != nil || outcome.Pending != nil {
		t.Fatalf("Run = %#v, %v", outcome, err)
	}
	before, err := os.Stat(activity)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(activity)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("detached group survived successful turn: activity grew from %d to %d bytes", before.Size(), after.Size())
	}
	assertChildReaped(t, pidPath)
}

func assertChildReaped(t *testing.T, pidPath string) {
	t.Helper()
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid child PID: %q", raw)
	}
	if _, err := readProcess(pid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child %d was not reaped: %v", pid, err)
	}
}

func TestDoubleForkCleanupWithResetEnvironment(t *testing.T) {
	for _, test := range []struct {
		name                             string
		inheritPipes, keepLeader, cancel bool
		grace, ceiling                   time.Duration
		wantError                        error
	}{
		{name: "completion", grace: time.Second, ceiling: 10 * time.Second},
		{name: "completion with inherited pipes", inheritPipes: true, grace: 200 * time.Millisecond, ceiling: 10 * time.Second},
		{name: "grace expiry", inheritPipes: true, keepLeader: true, grace: 200 * time.Millisecond, ceiling: 10 * time.Second},
		{name: "budget expiry", inheritPipes: true, keepLeader: true, grace: 10 * time.Second, ceiling: 200 * time.Millisecond, wantError: errExecutionBudgetExceeded},
		{name: "cancellation", inheritPipes: true, keepLeader: true, cancel: true, grace: 10 * time.Second, ceiling: 10 * time.Second, wantError: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			activity, pidPath := filepath.Join(dir, "activity"), filepath.Join(dir, "pid")
			grandchild := filepath.Join(dir, "grandchild")
			child := filepath.Join(dir, "child")
			if err := os.WriteFile(grandchild, []byte(`trap '' INT
printf '%s\n' "$$" > "$CHILD_PID"
while :; do printf x >> "$ACTIVITY"; sleep 0.02; done
`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(child, []byte("/bin/sh \"$GRANDCHILD\" &\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Reset the environment, create a separate session, and let its
			// launcher exit. Ownership must survive all three transitions.
			script := `setsid env -i PATH=/usr/bin:/bin ACTIVITY="$ACTIVITY" CHILD_PID="$CHILD_PID" GRANDCHILD="$GRANDCHILD" /bin/sh "$CHILD"`
			if !test.inheritPipes {
				script += " >/dev/null 2>&1"
			}
			script += ` &
while [ ! -s "$ACTIVITY" ]; do sleep 0.01; done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}' '{"type":"assistant","message":{"id":"msg_wait","content":[{"type":"text","text":"waiting after result"}]}}' '{"type":"result","subtype":"success","origin":{"kind":"task-notification"}}'
`
			if test.keepLeader {
				script += "trap '' INT\nexec sleep 30\n"
			}
			driver := scriptedDriver(t, script, "PATH=/usr/bin:/bin", "ACTIVITY="+activity, "CHILD_PID="+pidPath, "GRANDCHILD="+grandchild, "CHILD="+child)
			driver.config.PostResultGrace, driver.config.TurnTimeout = test.grace, test.ceiling
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			waiting := make(chan struct{})
			if test.cancel {
				go func() {
					select {
					case <-waiting:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			outcome, err := driver.Run(ctx, runtime.Turn{Prompt: "hello"}, waitingSink{EventSink: &recordingSink{}, waiting: waiting})
			if !errors.Is(err, test.wantError) || outcome.Failure != nil || outcome.Pending != nil {
				t.Fatalf("Run = %#v, %v; want %v", outcome, err, test.wantError)
			}
			assertChildReaped(t, pidPath)
		})
	}
}
