package runtime

import (
	"context"
	"errors"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
)

// Steer is a user message added to a turn that is already running. The turn
// that accepts it owns it: the turn ends only after the runtime has consumed
// every message it owns, or reports the ones it did not.
type Steer struct {
	TaskID    string
	MessageID string
	Text      string
}

// Steering is implemented by a Runner whose running turn accepts follow-up
// user messages. Acceptance is decided atomically with the turn's end: a
// steer is either owned by the running turn or refused, never left without a
// task.
type Steering interface {
	// Steer hands message to the running turn of TaskID. A message the turn
	// already owns is accepted again without being resent.
	Steer(context.Context, Steer) error
}

// SteerRefusal is why a steer was refused. Nothing was sent to the runtime.
type SteerRefusal string

const (
	// SteerTaskNotRunning means the task has no running turn to steer.
	SteerTaskNotRunning SteerRefusal = apia2a.SteerTaskNotRunning
	// SteerUnsupported means the runtime cannot add messages to a running
	// turn, such as Claude's process driver.
	SteerUnsupported SteerRefusal = apia2a.SteerUnsupported
	// SteerInputRequired means the turn waits for an input response.
	SteerInputRequired SteerRefusal = apia2a.SteerInputRequired
)

// ErrSteerRefused wraps every SteerRefusal.
var ErrSteerRefused = errors.New("steer refused")

func (r SteerRefusal) Error() string { return "steer refused: " + string(r) }
func (r SteerRefusal) Unwrap() error { return ErrSteerRefused }

// SteerSink is implemented by an EventSink that records the steers a turn
// accepted but did not consume. A runtime calls it once, before a turn that
// owns unconsumed steers returns, whatever the turn's outcome. A completed
// turn has consumed every steer it accepted and never reports any.
type SteerSink interface {
	SteersUnconsumed(messageIDs []string)
}
