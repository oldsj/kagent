package workspace

import (
	"context"
	"errors"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// The executor recovers a turn that was waiting for input from the runner it
// is given. With Git enabled that runner is the Bootstrapper.
func TestPendingInputIsRestoredThroughTheBootstrapper(t *testing.T) {
	t.Run("Given a driver holding a turn that waits for input, when the executor admits work after a restart, assert that the task still owns the Actor", func(t *testing.T) {
		next := &restoringRunner{taskID: "task-1", contextID: "context-1", pending: &waitingTurn{}}
		executor := newWrappedExecutor(t, next)

		reserved, err := executor.ReservedTaskID()

		if err != nil {
			t.Fatalf("ReservedTaskID() error = %v", err)
		}
		if reserved != a2atype.TaskID("task-1") {
			t.Fatalf("reserved task = %q, want the task whose input is pending", reserved)
		}
	})

	t.Run("Given a driver that cannot read its pending input, when the executor admits work after a restart, assert that the failure is reported", func(t *testing.T) {
		next := &restoringRunner{err: errors.New("pending input is unreadable")}
		executor := newWrappedExecutor(t, next)

		_, err := executor.ReservedTaskID()

		if !errors.Is(err, next.err) {
			t.Fatalf("ReservedTaskID() error = %v, want the driver's restore failure", err)
		}
	})

	t.Run("Given a driver without durable input, when the executor admits work after a restart, assert that the Actor is free", func(t *testing.T) {
		executor := newWrappedExecutor(t, &fakeRunner{})

		reserved, err := executor.ReservedTaskID()

		if err != nil || reserved != "" {
			t.Fatalf("ReservedTaskID() = %q, %v; want no reserved task", reserved, err)
		}
	})
}

func newWrappedExecutor(t *testing.T, next Runner) *runtimea2a.Executor {
	t.Helper()
	executor, err := runtimea2a.New(newBootstrapper(t, &fakeSource{}, next), &memoryContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

type restoringRunner struct {
	fakeRunner
	taskID, contextID string
	pending           runtime.PendingTurn
	err               error
}

func (r *restoringRunner) RestorePending() (string, string, runtime.PendingTurn, error) {
	return r.taskID, r.contextID, r.pending, r.err
}

type waitingTurn struct{}

func (*waitingTurn) Request() runtime.InputRequest {
	return &runtime.ApprovalRequest{ID: "request-1", CallID: "call-1", Name: "deploy"}
}

func (*waitingTurn) Resume(context.Context, runtime.InputResponse, runtime.EventSink) (runtime.Outcome, error) {
	return runtime.Outcome{}, nil
}

func (*waitingTurn) Cancel(context.Context, runtime.ModelCallSink) error { return nil }

type memoryContinuation struct{ id string }

func (c *memoryContinuation) Load() (string, bool, error) { return c.id, c.id != "", nil }
func (c *memoryContinuation) Bind(id string) error        { c.id = id; return nil }
