package a2a

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// outageStore rejects every update while the controller is unavailable, as the
// TaskStore did with PermissionDenied while the controller restarted.
type outageStore struct {
	*taskstore.InMemory
	down atomic.Bool
}

func (s *outageStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if s.down.Load() {
		return taskstore.TaskVersionMissing, a2atype.ErrUnauthorized
	}
	return s.InMemory.Update(ctx, update)
}

func submittedTask(t *testing.T, store taskstore.Store, id a2atype.TaskID) {
	t.Helper()
	if _, err := store.Create(t.Context(), &a2atype.Task{
		ID: id, ContextID: testContextID, Status: a2atype.TaskStatus{State: a2atype.TaskStateSubmitted},
	}); err != nil {
		t.Fatal(err)
	}
}

func userMessage(id a2atype.TaskID, text string) *a2atype.SendMessageRequest {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	message.TaskID, message.ContextID = id, testContextID
	return &a2atype.SendMessageRequest{Message: message}
}

// A save that fails mid-turn ends the execution without a final state, so the
// stored task stays working. Canceling it afterwards must settle it rather than
// leave the SDK waiting for an event that no execution will publish.
func TestCancelSettlesTaskLeftWorkingByFailedSave(t *testing.T) {
	store := &outageStore{InMemory: taskstore.NewInMemory(nil)}
	runnerReturned := make(chan struct{}, 1)
	executor, err := New(fakeRunner{run: func(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
		if turn.Prompt != "search" {
			return runtime.Outcome{}, nil
		}
		defer func() { runnerReturned <- struct{}{} }()
		if err := sink.ToolCall(runtime.ToolCall{ID: "toolu_1", Name: "WebSearch"}); err != nil {
			return runtime.Outcome{}, err
		}
		// Wait until the first tool call is saved, then lose the TaskStore.
		deadline := time.Now().Add(5 * time.Second)
		for {
			stored, err := store.Get(ctx, "task-1")
			if err == nil && len(stored.Task.Artifacts) == 1 {
				break
			}
			if time.Now().After(deadline) {
				return runtime.Outcome{}, context.DeadlineExceeded
			}
			time.Sleep(5 * time.Millisecond)
		}
		store.down.Store(true)
		if err := sink.ToolCall(runtime.ToolCall{ID: "toolu_2", Name: "WebSearch"}); err != nil {
			return runtime.Outcome{}, err
		}
		// The native tool calls never complete.
		<-ctx.Done()
		return runtime.Outcome{}, ctx.Err()
	}}, &fakeContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(store))
	submittedTask(t, store, "task-1")
	submittedTask(t, store, "task-2")

	if _, err := handler.SendMessage(t.Context(), userMessage("task-1", "search")); err == nil {
		t.Fatal("SendMessage() succeeded although its task could not be saved")
	}
	select {
	case <-runnerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed save did not stop the native turn")
	}
	stored, err := store.Get(t.Context(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Task.Status.State != a2atype.TaskStateWorking {
		t.Fatalf("task state after the failed save = %s, want working", stored.Task.Status.State)
	}

	// The controller is back. Cancellation settles the task promptly.
	store.down.Store(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	task, err := handler.CancelTask(ctx, &a2atype.CancelTaskRequest{ID: "task-1"})
	if err != nil {
		t.Fatalf("CancelTask() error = %v", err)
	}
	if task.Status.State != a2atype.TaskStateCanceled {
		t.Fatalf("CancelTask() state = %s, want canceled", task.Status.State)
	}
	stored, err = store.Get(t.Context(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Task.Status.State != a2atype.TaskStateCanceled {
		t.Fatalf("stored state = %s, want canceled", stored.Task.Status.State)
	}

	// The Actor accepts its next turn.
	result, err := handler.SendMessage(t.Context(), userMessage("task-2", "next"))
	if err != nil {
		t.Fatalf("next SendMessage() error = %v", err)
	}
	if next, ok := result.(*a2atype.Task); !ok || next.Status.State != a2atype.TaskStateCompleted {
		t.Fatalf("next SendMessage() = %#v, want a completed task", result)
	}
}

func TestCancelSettlesTaskNoExecutionOwns(t *testing.T) {
	executor, err := New(fakeRunner{run: func(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
		return runtime.Outcome{}, nil
	}}, &fakeContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	request := requestContext("task-1", "ignored")
	request.StoredTask = &a2atype.Task{ID: "task-1", ContextID: testContextID, Status: a2atype.TaskStatus{State: a2atype.TaskStateWorking}}
	events, errs := collect(executor.Cancel(t.Context(), request))
	if len(errs) != 0 || len(events) != 1 {
		t.Fatalf("Cancel() events/errors = %#v/%v", events, errs)
	}
	if status, ok := events[0].(*a2atype.TaskStatusUpdateEvent); !ok || status.Status.State != a2atype.TaskStateCanceled {
		t.Fatalf("Cancel() event = %#v, want canceled", events[0])
	}
	request.StoredTask.Status.State = a2atype.TaskStateCompleted
	if events, errs := collect(executor.Cancel(t.Context(), request)); len(events) != 0 || len(errs) != 0 {
		t.Fatalf("Cancel() of a finished task = %#v/%v, want nothing", events, errs)
	}
}
