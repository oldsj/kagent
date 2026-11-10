package driver

import "github.com/kagent-dev/kagent/go/harness/runtime"

type EventKind string

const (
	EventSessionStarted EventKind = "session_started"
	EventTextDelta      EventKind = "text_delta"
	EventToolActivity   EventKind = "tool_activity"
	EventCompleted      EventKind = "completed"
	EventFailed         EventKind = "failed"
	EventModelCall      EventKind = "model_call"
	EventDeferred       EventKind = "deferred"
	// EventBackgroundTasks reports a change in how many background tasks
	// Claude has not yet reported to the conversation.
	EventBackgroundTasks EventKind = "background_tasks"
	EventHealth          EventKind = "health"
)

// Event is the Claude stream vocabulary consumed by ProcessDriver. Vendor
// parsing details stay here and are normalized before reaching shared runtime
// code.
type Event struct {
	Kind      EventKind
	SessionID string
	Text      string
	ToolID    string
	ToolName  string
	ToolPhase string
	// ParentToolID is the Agent call whose subagent started the tool.
	ParentToolID string
	ToolResult   any
	ToolError    bool
	Metadata     map[string]any
	SafeMessage  string
	Deferred     *DeferredTool
	ModelCall    *runtime.ModelCall
	Health       runtime.HealthEvent
	// BackgroundTasks is the unreported background task count for
	// EventBackgroundTasks.
	BackgroundTasks int
}
