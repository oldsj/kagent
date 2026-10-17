package driver

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestFailedTurnDiagnostic(t *testing.T) {
	const authFailure = "Codex authentication failed; renew the configured model credential in the control plane."
	for _, test := range []struct {
		name    string
		failure *turnError
		want    string
	}{
		{name: "missing error", want: "Codex execution failed"},
		{name: "empty message", failure: &turnError{}, want: "Codex execution failed"},
		{name: "upstream detail", failure: &turnError{Message: "actor egress policy denied TLS destination"}, want: "Codex execution failed: actor egress policy denied TLS destination"},
		{name: "auth code", failure: &turnError{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL", CodexErrorInfo: json.RawMessage(`"unauthorized"`)}, want: authFailure},
		{name: "HTTP auth code", failure: &turnError{Message: "private response body", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream connection auth code", failure: &turnError{CodexErrorInfo: json.RawMessage(`{"responseStreamConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream disconnect auth code", failure: &turnError{CodexErrorInfo: json.RawMessage(`{"responseStreamDisconnected":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "retry auth code", failure: &turnError{CodexErrorInfo: json.RawMessage(`{"responseTooManyFailedAttempts":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "non-auth HTTP status", failure: &turnError{Message: "gateway refused the request", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":403}}`)}, want: "Codex execution failed: gateway refused the request"},
		{name: "null status", failure: &turnError{Message: "connection failed", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":null}}`)}, want: "Codex execution failed: connection failed"},
		{name: "unknown code", failure: &turnError{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"futureError"`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "unknown variant", failure: &turnError{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`{"futureError":{"httpStatusCode":401}}`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "credential withheld", failure: &turnError{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
		{name: "split credential indicator", failure: &turnError{Message: "Authori\nzation: SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
		{name: "unknown private code", failure: &turnError{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"SYNTHETIC_CREDENTIAL"`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "credential after truncation", failure: &turnError{Message: strings.Repeat("x", 2000) + " access_token=SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string     `json:"id"`
					Status string     `json:"status"`
					Error  *turnError `json:"error"`
				} `json:"turn"`
			}{ThreadID: "thread"}
			params.Turn.ID, params.Turn.Status, params.Turn.Error = "turn", "failed", test.failure
			raw, err := json.Marshal(params)
			require.NoError(t, err)
			translator := newEventTranslator("thread", "turn")
			translator.tools["active"] = activeTool{name: "command_execution"}
			sink := &recordingSink{}
			outcome, done, err := translator.translate(rpcMessage{Method: "turn/completed", Params: raw}, sink)
			require.NoError(t, err)
			require.True(t, done)
			require.NotNil(t, outcome.Failure)
			require.Equal(t, test.want, outcome.Failure.Message)
			require.NotContains(t, outcome.Failure.Message, "SYNTHETIC_CREDENTIAL")
			require.Len(t, sink.results, 1)
			require.True(t, sink.results[0].IsError)
		})
	}
}

func TestFailedTurnDiagnosticBound(t *testing.T) {
	message := codexFailureMessage(&turnError{Message: strings.Repeat("界", 1000)})
	require.LessOrEqual(t, len(message), 512)
	require.True(t, utf8.ValidString(message))
	require.True(t, strings.HasPrefix(message, "Codex execution failed: 界"))
	require.True(t, strings.HasSuffix(message, "..."))
}
