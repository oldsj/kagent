package a2a

import (
	"errors"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

const (
	// SteerMetadataKey marks a SendMessage that adds a user message to the
	// task named by its taskId while that task is running. A steer never
	// creates a task and is never queued: it is accepted into the running
	// turn or refused.
	SteerMetadataKey = "kagent.steer"
	// UnconsumedSteersMetadataKey lists, on a task that did not complete, the
	// message IDs of steers the task accepted but never consumed. A completed
	// task consumed every steer it accepted.
	UnconsumedSteersMetadataKey = "kagent.steer.unconsumed"
	// SteerRefusalMetadataKey names, in a refused steer's error details, why
	// the steer was refused.
	SteerRefusalMetadataKey = "steer"
	// SendNotAcceptedReason is the ErrorInfo reason of input that was not
	// accepted and left no record.
	SendNotAcceptedReason = "KAGENT_SEND_NOT_ACCEPTED"

	// SteerTaskNotRunning refuses a steer into a task with no running turn.
	SteerTaskNotRunning = "steer_task_not_running"
	// SteerUnsupported refuses a steer into a runtime that cannot add a
	// message to a running turn, such as Claude's process driver.
	SteerUnsupported = "steer_process_driver"
	// SteerInputRequired refuses a steer into a task waiting for input.
	SteerInputRequired = "steer_input_required"
)

// IsSteer reports whether message asks to steer a running task.
func IsSteer(message *a2atype.Message) bool {
	if message == nil {
		return false
	}
	steer, _ := message.Metadata[SteerMetadataKey].(bool)
	return steer
}

// SteerNotAccepted is the error a refused steer returns. The input left no
// record. Unlike a busy send it is not retried as is: the caller queues the
// message as a new turn instead.
func SteerNotAccepted(refusal string) *a2atype.Error {
	return a2atype.NewError(a2atype.ErrUnsupportedOperation, "steer was not accepted: "+refusal).
		WithErrorInfoMeta(map[string]string{"reason": SendNotAcceptedReason, SteerRefusalMetadataKey: refusal})
}

// SteerRefusal returns the refusal a SteerNotAccepted error carries.
func SteerRefusal(err error) (string, bool) {
	var protocolErr *a2atype.Error
	if !errors.As(err, &protocolErr) {
		return "", false
	}
	for _, detail := range protocolErr.TypedDetails {
		if detail.TypeURL != errordetails.ErrorInfoType {
			continue
		}
		var refusal string
		switch metadata := detail.Value["metadata"].(type) {
		case map[string]string:
			refusal = metadata[SteerRefusalMetadataKey]
		case map[string]any:
			refusal, _ = metadata[SteerRefusalMetadataKey].(string)
		}
		return refusal, refusal != ""
	}
	return "", false
}
