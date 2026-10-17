package driver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2alog "github.com/a2aproject/a2a-go/v2/log"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/stretchr/testify/require"
)

type failureContinuation struct{}

func (failureContinuation) Load() (string, bool, error) { return "", false, nil }
func (failureContinuation) Bind(string) error           { return nil }

func TestCodexFailureThroughA2A(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure nativeFailureFixture
		want    string
	}{
		{name: "native auth failure", failure: nativeFailureFixture{Message: "Bearer SYNTHETIC_CREDENTIAL", AdditionalDetails: "private native detail", CodexErrorInfo: json.RawMessage(`"unauthorized"`)}, want: "Codex authentication failed; renew the configured model credential in the control plane."},
		{name: "HTTP auth failure", failure: nativeFailureFixture{Message: "Bearer SYNTHETIC_CREDENTIAL", CodexErrorInfo: json.RawMessage(`{"httpConnectionFailed":{"httpStatusCode":401}}`)}, want: "Codex authentication failed; renew the configured model credential in the control plane."},
		{name: "unclassified upstream detail", failure: nativeFailureFixture{Message: "upstream unavailable"}, want: "Codex execution failed"},
		{name: "credential withheld", failure: nativeFailureFixture{Message: "Authorization: Custom SYNTHETIC_CREDENTIAL"}, want: "Codex execution failed"},
		{name: "long Unicode detail", failure: nativeFailureFixture{Message: strings.Repeat("界", 1000)}, want: "Codex execution failed"},
		{name: "signed URL query", failure: nativeFailureFixture{Message: "error sending request for url (https://upstream.example/responses?sig=Q7m9v2R8d4)"}, want: "Codex execution failed"},
		{name: "URL userinfo", failure: nativeFailureFixture{Message: "error sending request for url (https://alice:Q7m9v2R8d4@upstream.example/responses)"}, want: "Codex execution failed"},
		{name: "account identifier", failure: nativeFailureFixture{Message: `unexpected status 403 Forbidden: {"error":{"message":"Account 11111111-2222-4333-8444-555555555555 is disabled","account_id":"11111111-2222-4333-8444-555555555555"}}`}, want: "Codex execution failed"},
		{name: "unlabelled JWT", failure: nativeFailureFixture{Message: "unexpected status 403 Forbidden: invalid credential e30.eyJleHAiOjE5MDAwMDAwMDB9.c2lnbmF0dXJl"}, want: "Codex execution failed"},
		{name: "custom auth header", failure: nativeFailureFixture{Message: "unexpected status 403 Forbidden: X-Auth: Q7m9v2R8d4"}, want: "Codex execution failed"},
		{name: "unknown private category", failure: nativeFailureFixture{CodexErrorInfo: json.RawMessage(`"Q7m9v2R8d4"`)}, want: "Codex execution failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			failureJSON, err := json.Marshal(test.failure)
			require.NoError(t, err)
			terminal := `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"failed","error":` + string(failureJSON) + `}}}` + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "terminal"), []byte(terminal), 0o600))
			script := `#!/bin/sh
read initialize
printf '%s\n' '{"id":1,"result":{}}'
read initialized
read thread
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread"}}}'
read turn
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn"}}}'
cat terminal
while read ignored; do :; done
`
			executable := filepath.Join(dir, "codex")
			require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
			runner := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir, Model: "fixture-model", Provider: "fixture-provider",
				Environment: os.Environ(), MaxFrameBytes: 16384, MaxStderrBytes: 1024, InterruptGrace: time.Second,
			})
			executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
			require.NoError(t, err)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			ctx := logging.IntoContext(a2alog.AttachLogger(t.Context(), logger), logger)
			message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
			message.TaskID, message.ContextID = "task-failure", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
			var failed int
			var events bytes.Buffer
			for event, err := range executor.Execute(ctx, req) {
				require.NoError(t, err)
				require.NoError(t, json.NewEncoder(&events).Encode(event))
				update, ok := event.(*a2atype.TaskStatusUpdateEvent)
				if !ok {
					continue
				}
				require.NotEqual(t, a2atype.TaskStateCompleted, update.Status.State)
				if update.Status.State != a2atype.TaskStateFailed {
					continue
				}
				failed++
				require.NotNil(t, update.Status.Message)
				got := update.Status.Message.Parts[0].Text()
				require.Equal(t, test.want, got)
				require.LessOrEqual(t, len(got), 512)
				require.True(t, utf8.ValidString(got))
				require.NotContains(t, got, "SYNTHETIC_CREDENTIAL")
			}
			require.Equal(t, 1, failed)
			for _, private := range []string{"SYNTHETIC_CREDENTIAL", "Q7m9v2R8d4", "alice", "11111111-2222-4333-8444-555555555555", "e30.eyJleHAiOjE5MDAwMDAwMDB9.c2lnbmF0dXJl", "private native detail"} {
				require.NotContains(t, events.String(), private)
				require.NotContains(t, logs.String(), private)
			}
		})
	}
}
