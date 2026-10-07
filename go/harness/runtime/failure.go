package runtime

import "github.com/kagent-dev/kagent/go/harness/internal/utils"

// TerminalFailure is a runtime terminal failure with a vetted public message
// and a separately retained process cause. Its fields are private so callers
// cannot bypass message vetting.
type TerminalFailure struct {
	message string
	cause   error
}

// NewTerminalFailure vets the terminal diagnostic while preserving the cause
// for errors.Is and errors.As. Error deliberately excludes the raw cause.
func NewTerminalFailure(message string, cause error) *TerminalFailure {
	message = utils.SafeDiagnostic(message)
	if message == "" {
		message = "Harness runtime execution failed"
	}
	return &TerminalFailure{message: message, cause: cause}
}

func (t *TerminalFailure) Error() string { return t.PublicMessage() }

func (t *TerminalFailure) Unwrap() error { return t.cause }

func (t *TerminalFailure) PublicMessage() string {
	if t.message == "" {
		return "Harness runtime execution failed"
	}
	return t.message
}
