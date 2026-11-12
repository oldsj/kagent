package a2a

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestExecutorSteer(t *testing.T) {
	t.Run("Given a running task, when its runtime accepts a steer, assert that the steer enters the task history before the reply and the end", func(t *testing.T) {
		accepted := make(chan runtime.Steer, 1)
		var promptMessageID string
		f := newSteerExecutor(t, func(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
			promptMessageID = turn.MessageID
			<-accepted
			if err := sink.TextDelta(runtime.TextDelta{Text: "steered reply"}); err != nil {
				return runtime.Outcome{}, err
			}
			return runtime.Outcome{}, nil
		}, func(_ context.Context, steer runtime.Steer) error {
			accepted <- steer
			return nil
		})
		request := requestContext("task-1", "start")
		f.WhenExecuting(request)

		err := f.executor.Steer(t.Context(), "task-1", testContextID, steerMessage("m-steer", "use main"))

		if err != nil {
			t.Fatalf("Steer() error = %v", err)
		}
		events := f.AwaitEvents()
		steerAt := slices.IndexFunc(events, func(event a2atype.Event) bool {
			update, ok := event.(*a2atype.TaskStatusUpdateEvent)
			return ok && update.Status.Message != nil && update.Status.Message.ID == "m-steer" && update.Status.Message.Role == a2atype.MessageRoleUser
		})
		replyAt := slices.IndexFunc(events, func(event a2atype.Event) bool {
			_, ok := event.(*a2atype.TaskArtifactUpdateEvent)
			return ok
		})
		if steerAt < 0 || replyAt < steerAt || !isState(events[steerAt+1], a2atype.TaskStateWorking) || !isState(events[len(events)-1], a2atype.TaskStateCompleted) {
			t.Fatalf("events = %#v, want the steer, its archival, the reply, then completed", events)
		}
		if promptMessageID != request.Message.ID {
			t.Fatalf("turn message ID = %q, want %q", promptMessageID, request.Message.ID)
		}
	})

	t.Run("Given a running task, when its runtime refuses a steer, assert that the refusal reaches the caller and nothing enters history", func(t *testing.T) {
		release := make(chan struct{})
		f := newSteerExecutor(t, func(ctx context.Context, _ runtime.Turn, _ runtime.EventSink) (runtime.Outcome, error) {
			<-release
			return runtime.Outcome{}, nil
		}, func(context.Context, runtime.Steer) error { return runtime.SteerTaskNotRunning })
		f.WhenExecuting(requestContext("task-1", "start"))

		err := f.executor.Steer(t.Context(), "task-1", testContextID, steerMessage("m-steer", "late"))

		assertSteerRefusal(t, err, apia2a.SteerTaskNotRunning)
		close(release)
		for _, event := range f.AwaitEvents() {
			if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && update.Status.Message != nil && update.Status.Message.ID == "m-steer" {
				t.Fatalf("refused steer entered history: %#v", update)
			}
		}
	})

	t.Run("Given no running task, when a steer arrives, assert that it is refused as not running", func(t *testing.T) {
		f := newSteerExecutor(t, nil, func(context.Context, runtime.Steer) error {
			t.Fatal("the runtime must not be asked")
			return nil
		})

		err := f.executor.Steer(t.Context(), "task-1", testContextID, steerMessage("m-steer", "x"))

		assertSteerRefusal(t, err, apia2a.SteerTaskNotRunning)
	})

	t.Run("Given a task waiting for input, when a steer arrives, assert that it is refused as input required", func(t *testing.T) {
		f := newSteerExecutor(t, func(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
			return runtime.Outcome{Pending: &fakePendingTurn{
				request: &runtime.ApprovalRequest{ID: "approval-1", CallID: "call-1", Name: "protected.write", Hint: "Approve?"},
				cancel:  func(context.Context, runtime.ModelCallSink) error { return nil },
			}}, nil
		}, func(context.Context, runtime.Steer) error {
			t.Fatal("the runtime must not be asked")
			return nil
		})
		if _, errs := collect(f.executor.Execute(t.Context(), requestContext("task-1", "write"))); len(errs) != 0 {
			t.Fatalf("Execute() errors = %v", errs)
		}

		err := f.executor.Steer(t.Context(), "task-1", testContextID, steerMessage("m-steer", "x"))

		assertSteerRefusal(t, err, apia2a.SteerInputRequired)
	})

	t.Run("Given a runtime that cannot steer, when a steer arrives, assert that it is refused as unsupported", func(t *testing.T) {
		executor, err := New(fakeRunner{run: func(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
			return runtime.Outcome{}, nil
		}}, &fakeContinuation{}, tracing.RuntimeTelemetry{})
		if err != nil {
			t.Fatal(err)
		}

		err = executor.Steer(t.Context(), "task-1", testContextID, steerMessage("m-steer", "x"))

		assertSteerRefusal(t, err, apia2a.SteerUnsupported)
	})

	t.Run("Given a task whose runtime reports an unconsumed steer, when the task fails, assert that its final status lists the steer", func(t *testing.T) {
		f := newSteerExecutor(t, func(_ context.Context, _ runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
			sink.(runtime.SteerSink).SteersUnconsumed([]string{"m-steer"})
			return runtime.Outcome{Failure: &runtime.Failure{Message: "budget"}}, nil
		}, nil)

		events, errs := collect(f.executor.Execute(t.Context(), requestContext("task-1", "start")))

		if len(errs) != 0 || !isState(events[len(events)-1], a2atype.TaskStateFailed) {
			t.Fatalf("events/errors = %#v/%v, want failed", events, errs)
		}
		assertUnconsumed(t, events[len(events)-1], "m-steer")
	})

	t.Run("Given a running task whose runtime reports an unconsumed steer, when the task is canceled, assert that the canceled status lists the steer", func(t *testing.T) {
		f := newSteerExecutor(t, func(ctx context.Context, _ runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
			<-ctx.Done()
			sink.(runtime.SteerSink).SteersUnconsumed([]string{"m-steer"})
			return runtime.Outcome{}, ctx.Err()
		}, nil)
		f.WhenExecuting(requestContext("task-1", "start"))

		events, errs := collect(f.executor.Cancel(t.Context(), requestContext("task-1", "ignored")))

		if len(errs) != 0 || len(events) != 1 || !isState(events[0], a2atype.TaskStateCanceled) {
			t.Fatalf("Cancel() events/errors = %#v/%v", events, errs)
		}
		assertUnconsumed(t, events[0], "m-steer")
		f.AwaitEvents()
	})
}

type steeringRunner struct {
	fakeRunner
	steer func(context.Context, runtime.Steer) error
}

func (r steeringRunner) Steer(ctx context.Context, steer runtime.Steer) error {
	return r.steer(ctx, steer)
}

type steerExecutor struct {
	t        *testing.T
	executor *Executor
	started  chan struct{}
	events   chan []a2atype.Event
}

func newSteerExecutor(t *testing.T, run func(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error), steer func(context.Context, runtime.Steer) error) *steerExecutor {
	t.Helper()
	f := &steerExecutor{t: t, started: make(chan struct{}), events: make(chan []a2atype.Event, 1)}
	runner := steeringRunner{fakeRunner: fakeRunner{run: func(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
		close(f.started)
		return run(ctx, turn, sink)
	}}, steer: steer}
	executor, err := New(runner, &fakeContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	f.executor = executor
	return f
}

// WhenExecuting runs the task in the background until its runtime has started.
func (f *steerExecutor) WhenExecuting(request *a2asrv.ExecutorContext) {
	f.t.Helper()
	go func() {
		events, _ := collect(f.executor.Execute(context.Background(), request))
		f.events <- events
	}()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		f.t.Fatal("the runtime never started")
	}
}

func (f *steerExecutor) AwaitEvents() []a2atype.Event {
	f.t.Helper()
	select {
	case events := <-f.events:
		return events
	case <-time.After(5 * time.Second):
		f.t.Fatal("the task did not end")
		return nil
	}
}

func steerMessage(id, text string) *a2atype.Message {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	message.ID = id
	message.SetMeta(apia2a.SteerMetadataKey, true)
	return message
}

func isState(event a2atype.Event, state a2atype.TaskState) bool {
	update, ok := event.(*a2atype.TaskStatusUpdateEvent)
	return ok && update.Status.State == state
}

func assertSteerRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if got, ok := apia2a.SteerRefusal(err); !ok || got != want {
		t.Fatalf("err = %v, want a steer refusal %q", err, want)
	}
	if !errors.Is(err, a2atype.ErrUnsupportedOperation) {
		t.Fatalf("err = %v, want an unsupported-operation error", err)
	}
}

func assertUnconsumed(t *testing.T, event a2atype.Event, want ...string) {
	t.Helper()
	update := event.(*a2atype.TaskStatusUpdateEvent)
	got, _ := update.Metadata[apia2a.UnconsumedSteersMetadataKey].([]any)
	if len(got) != len(want) {
		t.Fatalf("unconsumed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unconsumed = %v, want %v", got, want)
		}
	}
}
