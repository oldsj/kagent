package driver

type EventKind string

const (
	EventSessionStarted EventKind = "session_started"
	EventTextDelta      EventKind = "text_delta"
	EventToolActivity   EventKind = "tool_activity"
	EventCompleted      EventKind = "completed"
	EventFailed         EventKind = "failed"
	// EventBackgroundTasks reports a change in the background work Claude
	// still owes the conversation a result for.
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
	// BackgroundTasks is the background work state for EventBackgroundTasks.
	BackgroundTasks BackgroundTasks
}

// BackgroundTasks summarizes Claude's background tasks whose outcome no result
// has reflected yet. Claude reports a finished task to the model in a
// follow-up iteration, and that iteration's result covers it.
type BackgroundTasks struct {
	// Live tasks are still running.
	Live int
	// Unreported tasks ended after the latest result.
	Unreported int
	// Stopped counts the Unreported tasks that Claude killed or stopped rather
	// than let finish, for example at print-mode wind-down or its idle ceiling.
	Stopped int
}

// owed reports whether Claude still has background work whose outcome should
// reach the conversation in a later iteration of this turn.
func (b BackgroundTasks) owed() bool {
	return b.Live > 0 || b.Unreported > 0
}

// lost counts tasks whose outcome can no longer reach the conversation once
// Claude has exited.
func (b BackgroundTasks) lost() int {
	return b.Live + b.Stopped
}
