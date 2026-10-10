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
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/stretchr/testify/require"
)

type failureContinuation struct{}

func (failureContinuation) Load() (string, bool, error) { return "", false, nil }
func (failureContinuation) Bind(string) error           { return nil }

func TestPostResultActivityCompletesA2AOnce(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile("../../testdata/stream-post-result.jsonl")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stream"), fixture, 0o600))
	executable := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ncat >/dev/null\ncat stream\n"), 0o700))
	runner := NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second,
	})
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	require.NoError(t, err)
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
	message.TaskID, message.ContextID = "task-post-result", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
	var completed int
	for event, err := range executor.Execute(t.Context(), req) {
		require.NoError(t, err)
		if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok {
			require.NotEqual(t, a2atype.TaskStateFailed, update.Status.State)
			if update.Status.State == a2atype.TaskStateCompleted {
				completed++
			}
		}
	}
	require.Equal(t, 1, completed)
}

func TestTerminalFailureThroughA2A(t *testing.T) {
	const detail = "API Error: 503 upstream unavailable"
	const withheld = "upstream error (details withheld: possible credential)"
	const truncated = "upstream error (details withheld: stderr truncated)"
	const multiline = "API Error: upstream echoed Authorization: \"Custom prefix\nQ7m9v2R8d4\""
	const longBounded = "API Error: " // 11 bytes leaves 1010 bytes for UTF-8 text.
	longDetail := longBounded + strings.Repeat("界", 1000)
	longWant := longBounded + strings.Repeat("界", 336) + "..."
	secretText := "Authorization: Custom Q7m9v2R8d4\n" +
		`api_key="Q7m9v2R8d4" token=Q7m9v2R8d4` + "\n" +
		strings.Repeat("long diagnostic ", 200)
	for _, test := range []struct {
		name       string
		detail     string
		stderr     string
		wantStderr string
		want       string
		exit       string
		omitResult bool
		wantError  string
	}{
		{name: "split API key", detail: "API\nkey is Q7m9v2R8d4", stderr: "API\nkey is Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "unicode private key", detail: "-----BEGIN PRIVATE\u00a0KEY-----\nQ7m9v2R8d4", stderr: "-----BEGIN PRIVATE\u00a0KEY-----\nQ7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "split authorization", detail: "Authori\nzation: Q7m9v2R8d4", stderr: "Authori\nzation: Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "unicode case folding", detail: "ſecret: Q7m9v2R8d4", stderr: "ſecret: Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "stderr indicator after capture", detail: detail, stderr: "Q7m9v2R8d4" + strings.Repeat(" ", 20000) + " (authorization)", wantStderr: truncated, want: detail, exit: "1"},
		{name: "stderr overflow without indicator", detail: detail, stderr: "Q7m9v2R8d4" + strings.Repeat(" ", 20000), wantStderr: truncated, want: detail, exit: "1"},
		{name: "missing terminal stderr overflow", omitResult: true, stderr: "Q7m9v2R8d4" + strings.Repeat(" ", 20000) + " (authorization)", want: "Harness runtime execution failed", wantError: "claude process exited without a terminal result event: " + truncated, exit: "1"},
		{name: "without stderr", detail: detail, want: detail, exit: "1"},
		{name: "with stderr", detail: detail, stderr: "proxy refused", wantStderr: "proxy refused", want: detail, exit: "1"},
		{name: "bare header name", detail: "API Error: 401 invalid x-api-key", stderr: "API Error: 401 invalid x-api-key", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "header with value", detail: "x-api-key: Q7m9v2R8d4", stderr: "x-api-key: Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "multiline authorization", detail: multiline, stderr: multiline, wantStderr: withheld, want: withheld, exit: "1"},
		{name: "quoted bearer", detail: `Bearer "Q7m9v2R8d4"`, stderr: `Bearer "Q7m9v2R8d4"`, wantStderr: withheld, want: withheld, exit: "1"},
		{name: "unquoted bearer", detail: "bEaReR Q7m9v2R8d4", stderr: "bEaReR Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "secrets and long text", detail: secretText, stderr: secretText, wantStderr: withheld, want: withheld, exit: "1"},
		{name: "indicator after limit", detail: longDetail + " token=Q7m9v2R8d4", stderr: longDetail + " token=Q7m9v2R8d4", wantStderr: withheld, want: withheld, exit: "1"},
		{name: "long clean diagnostic", detail: longDetail, stderr: longDetail, wantStderr: longWant, want: longWant, exit: "1"},
		{name: "zero exit withholds credential", detail: secretText, want: withheld, exit: "0"},
		{name: "zero exit keeps long failure fallback", detail: longDetail, want: "Harness runtime execution failed", exit: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			terminal, err := json.Marshal(struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				IsError bool   `json:"is_error"`
				Result  string `json:"result"`
			}{Type: "result", Subtype: "success", IsError: true, Result: test.detail})
			require.NoError(t, err)
			// Fixtures are synthetic text carried in files, never shell arguments.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "result"), append(terminal, '\n'), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "stderr"), []byte(test.stderr), 0o600))
			script := "#!/bin/sh\ncat stderr >&2\n"
			if !test.omitResult {
				script += "cat result\n"
			}
			script += "exit " + test.exit + "\n"
			require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
			runner := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir, MaxEventBytes: 16384,
				MaxStderrBytes: 16384, InterruptGrace: time.Second,
			})
			executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
			require.NoError(t, err)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			ctx := logging.IntoContext(a2alog.AttachLogger(t.Context(), logger), logger)
			message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
			message.TaskID, message.ContextID = "task-failure", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
			var last *a2atype.TaskStatusUpdateEvent
			for event, err := range executor.Execute(ctx, req) {
				require.NoError(t, err)
				if update, ok := event.(*a2atype.TaskStatusUpdateEvent); ok {
					last = update
				}
			}
			require.NotNil(t, last)
			require.Equal(t, a2atype.TaskStateFailed, last.Status.State)
			require.NotNil(t, last.Status.Message)
			got := last.Status.Message.Parts[0].Text()
			require.Equal(t, test.want, got)
			require.LessOrEqual(t, len(got), utils.MaxDiagnosticBytes)
			require.True(t, utf8.ValidString(got))
			require.NotContains(t, got, "Q7m9v2R8d4")
			require.NotContains(t, logs.String(), "Q7m9v2R8d4")
			if test.exit != "0" {
				require.Contains(t, logs.String(), "Harness runtime execution failed")
			}
			var stderrLogged bool
			var failureLogged bool
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				if line == "" {
					continue
				}
				var record struct {
					Level  string `json:"level"`
					Stderr string `json:"stderr"`
					Error  string `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				require.LessOrEqual(t, len(record.Error), utils.MaxDiagnosticBytes)
				require.LessOrEqual(t, len(record.Stderr), utils.MaxDiagnosticBytes)
				if record.Level == "ERROR" {
					failureLogged = true
					wantError := test.wantError
					if wantError == "" {
						wantError = test.want
					}
					require.Equal(t, wantError, record.Error)
				}
				if record.Stderr != "" {
					stderrLogged = true
					require.Equal(t, "WARN", record.Level)
					require.Equal(t, test.wantStderr, record.Stderr)
				}
			}
			require.Equal(t, test.stderr != "" && !test.omitResult, stderrLogged)
			require.Equal(t, test.exit != "0", failureLogged)
		})
	}
}
