package driver

type EventKind string

const (
	EventSessionStarted EventKind = "session_started"
	EventTextDelta      EventKind = "text_delta"
	EventToolActivity   EventKind = "tool_activity"
	EventCompleted      EventKind = "completed"
	EventFailed         EventKind = "failed"
	// EventBackgroundTasks reports a change in how many background tasks
	// Claude is still running for the conversation.
	EventBackgroundTasks EventKind = "background_tasks"
)

// Event is the Claude stream vocabulary consumed by ProcessDriver. Vendor
// parsing details stay here and are normalized before reaching shared runtime
// code.
type Event struct {
	Kind        EventKind
	SessionID   string
	Text        string
	ToolID      string
	ToolName    string
	ToolPhase   string
	ToolResult  any
	ToolError   bool
	Metadata    map[string]any
	Category    string
	SafeMessage string
	Result      string
	// BackgroundTasks is the live background task count for
	// EventBackgroundTasks.
	BackgroundTasks int
}
