package a2a

import (
	"context"
	"errors"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// Steer adds message to the task this Actor is running. The runner accepts it
// into the running turn, or it is refused with apia2a.SteerNotAccepted. An
// accepted message enters the task's history from the execution goroutine
// before the task's next event.
func (e *Executor) Steer(ctx context.Context, taskID a2atype.TaskID, contextID string, message *a2atype.Message) error {
	err := e.steer(ctx, taskID, contextID, message)
	var refusal runtime.SteerRefusal
	if errors.As(err, &refusal) {
		return apia2a.SteerNotAccepted(string(refusal))
	}
	return err
}

func (e *Executor) steer(ctx context.Context, taskID a2atype.TaskID, contextID string, message *a2atype.Message) error {
	text, err := steerText(message)
	if err != nil {
		return err
	}
	steering, ok := e.runner.(runtime.Steering)
	if !ok {
		return runtime.SteerUnsupported
	}
	ref := taskRef{taskID: taskID, contextID: contextID}
	e.mu.Lock()
	if err := e.restorePending(); err != nil {
		e.mu.Unlock()
		return err
	}
	var sink *executionSink
	var refusal error = runtime.SteerTaskNotRunning
	switch state := e.state.(type) {
	case *activeTask:
		if state.matches(ref) && !state.cancelRequested {
			sink, refusal = state.sink, nil
		}
	case *parkedTask:
		if state.matches(ref) {
			refusal = runtime.SteerInputRequired
		}
	}
	e.mu.Unlock()
	if refusal != nil {
		return refusal
	}
	// The execution cannot flush this sink's steers, and so cannot publish its
	// final status, while the runner decides. A steer the runner accepts is
	// therefore in the history before the event that ends its task.
	sink.steerMu.Lock()
	defer sink.steerMu.Unlock()
	if err := steering.Steer(ctx, runtime.Steer{TaskID: string(taskID), MessageID: message.ID, Text: text}); err != nil {
		return err
	}
	if !containsMessage(sink.steers, message.ID) {
		steer := *message
		steer.TaskID, steer.ContextID = taskID, contextID
		sink.steers = append(sink.steers, &steer)
	}
	return nil
}

// steerText is the one text part a steer carries, as for a turn's prompt.
func steerText(message *a2atype.Message) (string, error) {
	if message == nil || message.ID == "" || message.Role != a2atype.MessageRoleUser {
		return "", a2atype.NewError(a2atype.ErrInvalidParams, "a steer is a user message with an ID")
	}
	if len(message.Parts) != 1 || message.Parts[0] == nil || message.Parts[0].Text() == "" {
		return "", a2atype.NewError(a2atype.ErrInvalidParams, "a steer carries exactly one non-empty text part")
	}
	return message.Parts[0].Text(), nil
}

func containsMessage(messages []*a2atype.Message, id string) bool {
	for _, message := range messages {
		if message.ID == id {
			return true
		}
	}
	return false
}

// flushSteers publishes accepted steers into the task's history. A status
// update carrying the message, followed by one without, archives the message
// in history; a2a-go accepts no bare message once the task exists. The text
// that follows starts a new artifact, ordered after the steer that prompted it.
func (s *executionSink) flushSteers() error {
	s.steerMu.Lock()
	steers := s.steers
	s.steers = nil
	s.steerMu.Unlock()
	for _, steer := range steers {
		apia2a.SetTimelinePosition(steer, s.nextTimelinePosition())
		if !s.yield(a2atype.NewStatusUpdateEvent(s.reqCtx, a2atype.TaskStateWorking, steer), nil) ||
			!s.yield(a2atype.NewStatusUpdateEvent(s.reqCtx, a2atype.TaskStateWorking, nil), nil) {
			return errYieldStopped
		}
		s.textArtifactID = ""
	}
	return nil
}

// SteersUnconsumed records the steers the turn accepted but did not consume.
func (s *executionSink) SteersUnconsumed(messageIDs []string) {
	s.unconsumed = append(s.unconsumed, messageIDs...)
}

func (s *executionSink) withUnconsumed(event *a2atype.TaskStatusUpdateEvent) *a2atype.TaskStatusUpdateEvent {
	return withUnconsumed(event, s.unconsumed)
}

// withUnconsumed lists on a final status the steers its task did not consume.
// Status metadata is merged into the task's metadata, where callers read it.
func withUnconsumed(event *a2atype.TaskStatusUpdateEvent, unconsumed []string) *a2atype.TaskStatusUpdateEvent {
	if len(unconsumed) == 0 {
		return event
	}
	ids := make([]any, len(unconsumed))
	for i, id := range unconsumed {
		ids[i] = id
	}
	if event.Metadata == nil {
		event.Metadata = map[string]any{}
	}
	event.Metadata[apia2a.UnconsumedSteersMetadataKey] = ids
	return event
}

var _ runtime.SteerSink = (*executionSink)(nil)
