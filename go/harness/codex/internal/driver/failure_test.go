package driver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Native input includes untrusted fields that the production decoder omits.
type nativeFailureFixture struct {
	Message           string          `json:"message"`
	AdditionalDetails string          `json:"additionalDetails,omitempty"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
}

func TestFailedTurnDiagnostic(t *testing.T) {
	const authFailure = "Codex authentication failed; renew the configured model credential in the control plane."
	for _, test := range []struct {
		name    string
		failure *nativeFailureFixture
		want    string
	}{
		{name: "missing error", want: "Codex execution failed"},
		{name: "empty message", failure: &nativeFailureFixture{}, want: "Codex execution failed"},
		{name: "upstream detail", failure: &nativeFailureFixture{Message: "actor egress policy denied TLS destination"}, want: "Codex execution failed"},
		{name: "auth code", failure: &nativeFailureFixture{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL", AdditionalDetails: "private account data", CodexErrorInfo: json.RawMessage(`"unauthorized"`)}, want: authFailure},
		{name: "HTTP auth code", failure: &nativeFailureFixture{Message: "private response body", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream connection auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseStreamConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream disconnect auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseStreamDisconnected":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "retry auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseTooManyFailedAttempts":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "non-auth HTTP status", failure: &nativeFailureFixture{Message: "gateway refused the request", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":403}}`)}, want: "Codex execution failed"},
		{name: "null status", failure: &nativeFailureFixture{Message: "connection failed", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":null}}`)}, want: "Codex execution failed"},
		{name: "unknown code", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"futureError"`)}, want: "Codex execution failed"},
		{name: "unknown variant", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`{"futureError":{"httpStatusCode":401}}`)}, want: "Codex execution failed"},
		{name: "credential withheld", failure: &nativeFailureFixture{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed"},
		{name: "split credential indicator", failure: &nativeFailureFixture{Message: "Authori\nzation: SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed"},
		{name: "unknown private code", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"SYNTHETIC_CREDENTIAL"`)}, want: "Codex execution failed"},
		{name: "long untrusted text", failure: &nativeFailureFixture{Message: strings.Repeat("界", 2000) + " access_token=SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed"},
		{name: "auth-like free text", failure: &nativeFailureFixture{Message: "unauthorized HTTP 401; renew auth"}, want: "Codex execution failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string                `json:"id"`
					Status string                `json:"status"`
					Error  *nativeFailureFixture `json:"error"`
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
