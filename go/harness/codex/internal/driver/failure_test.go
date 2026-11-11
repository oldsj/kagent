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
		{name: "upstream detail", failure: &nativeFailureFixture{Message: "actor egress policy denied TLS destination"}, want: "Codex execution failed: actor egress policy denied TLS destination"},
		{name: "auth code", failure: &nativeFailureFixture{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL", AdditionalDetails: "private account data", CodexErrorInfo: json.RawMessage(`"unauthorized"`)}, want: authFailure},
		{name: "HTTP auth code", failure: &nativeFailureFixture{Message: "private response body", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream connection auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseStreamConnectionFailed":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "stream disconnect auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseStreamDisconnected":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "retry auth code", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseTooManyFailedAttempts":{"httpStatusCode":401}}`)}, want: authFailure},
		{name: "non-auth HTTP status", failure: &nativeFailureFixture{Message: "gateway refused the request", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":403}}`)}, want: "Codex execution failed: upstream status 403: gateway refused the request"},
		{name: "null status", failure: &nativeFailureFixture{Message: "connection failed", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":null}}`)}, want: "Codex execution failed: connection failed"},
		{name: "unknown code", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"futureError"`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "unknown variant", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`{"futureError":{"httpStatusCode":401}}`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "credential withheld", failure: &nativeFailureFixture{Message: "Authorization: Bearer SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
		{name: "split credential indicator", failure: &nativeFailureFixture{Message: "Authori\nzation: SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
		{name: "unknown private code", failure: &nativeFailureFixture{Message: "provider is unavailable", CodexErrorInfo: json.RawMessage(`"SYNTHETIC_CREDENTIAL"`)}, want: "Codex execution failed: provider is unavailable"},
		{name: "long untrusted text", failure: &nativeFailureFixture{Message: strings.Repeat("界", 2000) + " access_token=SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed: upstream error (details withheld: possible credential)"},
		{name: "auth-like free text", failure: &nativeFailureFixture{Message: "unauthorized HTTP 401; renew auth"}, want: "Codex execution failed: unauthorized HTTP 401; renew auth"},
		{name: "upstream JSON error", failure: &nativeFailureFixture{Message: `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'not-a-real-model-xyz' model is not supported when using Codex with a ChatGPT account."}}`, CodexErrorInfo: json.RawMessage(`"other"`)}, want: "Codex execution failed: upstream status 400: invalid_request_error: The 'not-a-real-model-xyz' model is not supported when using Codex with a ChatGPT account."},
		{name: "unexpected status with detail body", failure: &nativeFailureFixture{Message: `unexpected status 400 Bad Request: {"detail":"Unsupported model gpt-5.5-codex"}`}, want: "Codex execution failed: upstream status 400: Unsupported model gpt-5.5-codex"},
		{name: "status without text", failure: &nativeFailureFixture{CodexErrorInfo: json.RawMessage(`{"responseStreamDisconnected":{"httpStatusCode":502}}`)}, want: "Codex execution failed: upstream status 502"},
		{name: "body sibling fields dropped", failure: &nativeFailureFixture{Message: `{"status":403,"error":{"message":"Workspace is disabled"},"account_id":"acct_SYNTHETIC_CREDENTIAL"}`}, want: "Codex execution failed: upstream status 403: Workspace is disabled"},
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
