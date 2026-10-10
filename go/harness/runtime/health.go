package runtime

import "time"

const HealthSchema = "kagent.dev/orchestration-health/v1"

// HealthEvent contains only allowlisted observations. The A2A sink supplies the
// gateway task/session identity; product attribution belongs to the consumer.
type HealthEvent struct {
	SchemaVersion    string       `json:"schema_version"`
	EventID          string       `json:"event_id"`
	ProducerEpoch    string       `json:"producer_epoch"`
	Sequence         int64        `json:"sequence"`
	Kind             string       `json:"kind"`
	OccurredAt       time.Time    `json:"occurred_at"`
	TimeSource       string       `json:"time_source"`
	TimePrecision    string       `json:"time_precision"`
	Provider         string       `json:"provider"`
	RuntimeVersion   string       `json:"runtime_version"`
	Source           string       `json:"source"`
	TurnKey          string       `json:"turn_key"`
	RuntimeSessionID string       `json:"runtime_session_id"`
	A2ATaskID        string       `json:"a2a_task_id"`
	Turn             *HealthTurn  `json:"turn,omitempty"`
	Tool             *HealthTool  `json:"tool,omitempty"`
	Usage            *HealthUsage `json:"usage,omitempty"`
}

type HealthTurn struct {
	State    string `json:"state"`
	Coverage string `json:"coverage"`
}

type HealthTool struct {
	CallKey      string  `json:"call_key"`
	Phase        string  `json:"phase"`
	ToolClass    string  `json:"tool_class"`
	OperationKey string  `json:"operation_key"`
	Action       string  `json:"action"`
	Path         *string `json:"path"`
	Outcome      string  `json:"outcome"`
	Category     string  `json:"category"`
}

// A result is a turn-total sample, never added to assistant-response usage.
// Multiple result iterations replace the sample and remain partial until the
// pinned CLI's accounting across native children is qualified.
type HealthUsage struct {
	Scope        string `json:"scope"`
	Basis        string `json:"basis"`
	Inclusion    string `json:"inclusion"`
	SampleKey    string `json:"sample_key"`
	Revision     int64  `json:"revision"`
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	Coverage     string `json:"coverage"`
}
