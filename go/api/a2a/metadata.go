package a2a

import (
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
)

const (
	MetadataPrefix              = "kagent.dev/a2a/"
	TimelinePositionMetadataKey = MetadataPrefix + "timeline-position"
	TaskCreatedAtMetadataKey    = MetadataPrefix + "task-created-at"
	PartTypeMetadataKey         = MetadataPrefix + "part-type"
	UsageMetadataKey            = MetadataPrefix + "usage"

	// ModelMetadataKey and EffortMetadataKey carry the caller's optional
	// per-turn model and reasoning-effort overrides on the message that starts
	// a turn. Unlike the gateway-owned keys above, these are caller-supplied
	// input, not derived state, so they live outside the kagent.dev/a2a/
	// namespace and SanitizeCallerRequest never strips them.
	ModelMetadataKey  = "kagent.dev/model"
	EffortMetadataKey = "kagent.dev/effort"
)

// SetTimelinePosition records the temporary task-timeline ordering key.
func SetTimelinePosition(carrier a2atype.MetadataCarrier, position time.Time) {
	carrier.SetMeta(TimelinePositionMetadataKey, position.UTC().Format(time.RFC3339Nano))
}

// TimelinePosition reads the temporary task-timeline ordering key.
func TimelinePosition(carrier a2atype.MetadataCarrier) (time.Time, bool) {
	if carrier == nil {
		return time.Time{}, false
	}
	value, ok := carrier.Meta()[TimelinePositionMetadataKey].(string)
	if !ok {
		return time.Time{}, false
	}
	position, err := time.Parse(time.RFC3339Nano, value)
	return position, err == nil
}

// SetTaskCreatedAt records the durable creation time used by task projections.
func SetTaskCreatedAt(task *a2atype.Task, createdAt time.Time) {
	if task != nil {
		task.SetMeta(TaskCreatedAtMetadataKey, createdAt.UTC().Format(time.RFC3339Nano))
	}
}

// TaskCreatedAt reads the durable task creation time.
func TaskCreatedAt(task *a2atype.Task) (time.Time, bool) {
	if task == nil {
		return time.Time{}, false
	}
	value, ok := task.Metadata[TaskCreatedAtMetadataKey].(string)
	if !ok {
		return time.Time{}, false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, value)
	return createdAt, err == nil
}

// TurnModel reads the optional per-turn model override from message metadata.
func TurnModel(message *a2atype.Message) (string, bool) {
	return stringMeta(message, ModelMetadataKey)
}

// TurnEffort reads the optional per-turn reasoning-effort override from
// message metadata.
func TurnEffort(message *a2atype.Message) (string, bool) {
	return stringMeta(message, EffortMetadataKey)
}

func stringMeta(message *a2atype.Message, key string) (string, bool) {
	if message == nil {
		return "", false
	}
	value, ok := message.Metadata[key].(string)
	return value, ok
}

// SanitizeCallerRequest removes private state and metadata owned by the gateway.
func SanitizeCallerRequest(req *a2atype.SendMessageRequest) {
	if req == nil || req.Message == nil {
		return
	}
	clearGatewayMetadata(req.Metadata)
	clearGatewayMessageMetadata(req.Message)
}

func clearGatewayMessageMetadata(message *a2atype.Message) {
	if message == nil {
		return
	}
	clearGatewayMetadata(message.Metadata)
	for _, part := range message.Parts {
		if part != nil {
			clearGatewayMetadata(part.Metadata)
		}
	}
}

func clearGatewayMetadata(metadata map[string]any) {
	delete(metadata, TimelinePositionMetadataKey)
	delete(metadata, TaskCreatedAtMetadataKey)
}
