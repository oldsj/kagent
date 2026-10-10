package driver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"go.opentelemetry.io/otel/trace"
)

type recordingSink struct {
	health   []runtime.HealthEvent
	sessions []runtime.SessionStarted
	text     strings.Builder
	calls    []runtime.ToolCall
	results  []runtime.ToolResult
}

func (s *recordingSink) Health(event runtime.HealthEvent) error {
	s.health = append(s.health, event)
	return nil
}

func (s *recordingSink) SessionStarted(event runtime.SessionStarted) error {
	s.sessions = append(s.sessions, event)
	return nil
}
func (s *recordingSink) TextDelta(event runtime.TextDelta) error {
	s.text.WriteString(event.Text)
	return nil
}
func (s *recordingSink) ToolCall(event runtime.ToolCall) error {
	s.calls = append(s.calls, event)
	return nil
}
func (s *recordingSink) ToolResult(event runtime.ToolResult) error {
	s.results = append(s.results, event)
	return nil
}

func TestProcessDriverConsumesActivityAfterResult(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/stream-post-result.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		stream  string
		failure bool
	}{
		{name: "success then success", stream: string(fixture)},
		{name: "failure then success", stream: strings.Replace(string(fixture), `"subtype":"success","result":"first"`, `"subtype":"error_during_execution","result":"first"`, 1)},
		{name: "success then failure", stream: strings.Replace(string(fixture), `"subtype":"success","result":"last"`, `"subtype":"error_during_execution","result":"last"`, 1), failure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			streamPath := filepath.Join(dir, "stream.jsonl")
			if err := os.WriteFile(streamPath, []byte(test.stream), 0o600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(dir, "claude")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\ncat >/dev/null\ncat \"$STREAM\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			driver := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir, Environment: []string{"STREAM=" + streamPath},
				MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond,
			})
			sink := &recordingSink{}
			outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, sink)
			if err != nil || outcome.Pending != nil || (outcome.Failure != nil) != test.failure {
				t.Fatalf("Run() = %#v, %v", outcome, err)
			}
			if test.failure && outcome.Failure.Message != "last" {
				t.Fatalf("failure = %q, want last result", outcome.Failure.Message)
			}
			if sink.text.String() != "firstlast" || len(sink.sessions) != 1 || len(sink.calls) != 1 || len(sink.results) != 1 {
				t.Fatalf("streamed events = %#v, text = %q", sink, sink.text.String())
			}
		})
	}
}

func TestProcessDriverDeadlineAfterResult(t *testing.T) {
	for _, closeOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream remains open", true: "stream closed before process exit"}[closeOutput], func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			script := "#!/bin/sh\ntrap '' INT\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\"}' '{\"type\":\"assistant\",\"message\":{\"id\":\"msg_waiting\",\"content\":[{\"type\":\"text\",\"text\":\"waiting\"}]}}'\n"
			if closeOutput {
				script += "exec 1>&-\n"
			}
			script += "exec sleep 30\n"
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			driver := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			sink := &recordingSink{}
			outcome, err := driver.Run(ctx, runtime.Turn{Prompt: "hello"}, sink)
			if !errors.Is(err, context.DeadlineExceeded) || outcome.Pending != nil {
				t.Fatalf("Run() = %#v, %v, want deadline exceeded", outcome, err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("deadline did not reap the process promptly")
			}
			if sink.text.String() != "waiting" {
				t.Fatal("deadline fired before the result and subsequent activity were consumed")
			}
		})
	}
}

func TestResumedEventSinkDropsOnlyInterruptedResponseWarning(t *testing.T) {
	underlying := &recordingSink{}
	sink := resumedEventSink{EventSink: underlying}

	if err := sink.TextDelta(runtime.TextDelta{Text: "\n" + interruptedResponseWarning + "\n"}); err != nil {
		t.Fatal(err)
	}
	if err := sink.TextDelta(runtime.TextDelta{Text: "continued"}); err != nil {
		t.Fatal(err)
	}
	if underlying.text.String() != "continued" {
		t.Fatalf("resumed text = %q, want continued", underlying.text.String())
	}

	if _, err := emitEvent(Event{Kind: EventTextDelta, Text: interruptedResponseWarning}, underlying); err != nil {
		t.Fatal(err)
	}
	if underlying.text.String() != "continued"+interruptedResponseWarning {
		t.Fatalf("ordinary text = %q, want the vendor warning preserved", underlying.text.String())
	}
}

func TestTraceEnvironment(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatal(err)
	}
	state, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	ctxWithTraceState := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, TraceState: state,
	}))
	ctxWithoutTraceState := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	for _, test := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{
			name: "replace stale trace context",
			ctx:  ctxWithTraceState,
			want: []string{
				"PATH=/bin",
				"TRACEPARENT=00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
				"TRACESTATE=vendor=value",
			},
		},
		{
			name: "remove stale trace state",
			ctx:  ctxWithoutTraceState,
			want: []string{
				"PATH=/bin",
				"TRACEPARENT=00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
			},
		},
		{name: "remove stale trace context", ctx: t.Context(), want: []string{"PATH=/bin"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := []string{"PATH=/bin", "TRACEPARENT=stale", "TRACESTATE=stale"}
			got := traceEnvironment(test.ctx, environment)
			if !slices.Equal(got, test.want) {
				t.Fatalf("trace environment = %q, want %q", got, test.want)
			}
			wantInput := []string{"PATH=/bin", "TRACEPARENT=stale", "TRACESTATE=stale"}
			if !slices.Equal(environment, wantInput) {
				t.Fatalf("input environment mutated to %q", environment)
			}
		})
	}
}

func TestProcessDriverArgumentsAndStream(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	executable := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.260 (Claude Code)'; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$CAPTURE\"\nIFS= read -r line\nprintf '%s\\n' \"$line\" > \"$CAPTURE.stdin\"\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}'\ncat >/dev/null\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	agentsJSON := `{"reviewer":{"description":"Reviews changes","prompt":"Review carefully","tools":["Read"]}}`
	mcpConfigPath := filepath.Join(dir, "mcp.json")
	d := NewProcessDriver(ProcessConfig{Executable: executable, ExpectedVersion: pinnedClaudeVersion, StrictVersion: true, Workspace: dir, Model: "claude-test", AppendSystemPrompt: "extra", AgentsJSON: agentsJSON, MCPConfigPath: mcpConfigPath, PluginDirs: []string{filepath.Join(dir, "plugin-a")}, Environment: []string{"CAPTURE=" + capture}, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second})
	if err := d.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	sink := &recordingSink{}
	turn := runtime.Turn{Prompt: "hello", ContinuationID: "11111111-1111-4111-8111-111111111111"}
	outcome, err := d.Run(t.Context(), turn, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Failure != nil {
		t.Fatalf("Run() outcome = %#v", outcome)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(d.Args(turn), "\n") + "\n"
	if string(args) != want {
		t.Errorf("arguments = %q, want %q", args, want)
	}
	if strings.Contains(string(args), turn.Prompt) {
		t.Error("arguments carry the prompt, which belongs on stdin")
	}
	input, err := os.ReadFile(capture + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"user","message":{"role":"user","content":"hello"}}` + "\n"; string(input) != want {
		t.Errorf("stdin = %q, want %q", input, want)
	}
	for _, required := range []string{
		"--dangerously-skip-permissions\n",
		"--strict-mcp-config\n",
		"--input-format\nstream-json\n",
	} {
		if !strings.Contains(string(args), required) {
			t.Errorf("arguments do not contain required fixed policy flag %q", strings.TrimSpace(required))
		}
	}
	if !strings.Contains(string(args), "--agents\n"+agentsJSON+"\n") {
		t.Error("arguments do not contain compiler-owned local agents JSON")
	}
	if !strings.Contains(string(args), "--mcp-config\n"+mcpConfigPath+"\n") {
		t.Error("arguments do not contain compiler-owned MCP configuration")
	}
	if !strings.Contains(string(args), "--plugin-dir\n"+filepath.Join(dir, "plugin-a")+"\n") {
		t.Error("arguments do not load the native plugin directory")
	}
	if strings.Contains(string(args), "--permission-prompt-tool\n") {
		t.Error("arguments unexpectedly configure Claude's native permission bridge")
	}
	if len(sink.sessions) != 1 || sink.sessions[0].ContinuationID != turn.ContinuationID {
		t.Errorf("session events = %#v", sink.sessions)
	}
}

func TestProcessDriverParserFailureIncludesStderr(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
	}{
		{name: "exit before result", script: "echo 'resume failed' >&2\nexit 17\n"},
		{name: "malformed output from live process", script: "echo 'resume failed' >&2\necho 'invalid json'\nexec sleep 30\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+test.script), 0o700); err != nil {
				t.Fatal(err)
			}
			d := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir,
				MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond,
			})
			started := time.Now()
			_, err := d.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{})
			if err == nil || !strings.Contains(err.Error(), "resume failed") {
				t.Fatalf("Run() error = %v, want subprocess stderr", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("parser failure waited for the live subprocess to exit")
			}
		})
	}
}

func TestProcessDriverNonZeroExitKeepsTerminalFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		script     string
		want       []string
		wantAbsent []string
	}{
		{
			name:       "error result and empty stderr",
			script:     `printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"result":"API Error: 401 invalid x-api-key"}'` + "\nexit 1\n",
			want:       []string{"upstream error (details withheld: possible credential)"},
			wantAbsent: []string{"stderr:"},
		},
		{
			name:       "error result and stderr",
			script:     `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"turn failed"}'` + "\necho 'proxy refused' >&2\nexit 1\n",
			want:       []string{"turn failed"},
			wantAbsent: []string{"proxy refused"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+test.script), 0o700); err != nil {
				t.Fatal(err)
			}
			d := NewProcessDriver(ProcessConfig{
				Executable: executable, Workspace: dir,
				MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second,
			})
			_, err := d.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{})
			if err == nil {
				t.Fatal("Run() error = nil, want the non-zero exit")
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("Run() error = %v, want wrapped exit code 1", err)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Run() error = %q, want it to contain %q", err, want)
				}
			}
			for _, absent := range test.wantAbsent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("Run() error = %q, want it to omit %q", err, absent)
				}
			}
		})
	}
}

func TestExitError(t *testing.T) {
	waitErr := errors.New("exit status 1")
	failure := &runtime.Outcome{Failure: &runtime.Failure{Message: "boom"}}
	for _, test := range []struct {
		name     string
		terminal *runtime.Outcome
		stderr   string
		want     string
	}{
		{name: "nothing known", want: "claude exited with an error: exit status 1"},
		{name: "stderr only", stderr: "oops", want: "claude exited with an error: exit status 1: oops"},
		{name: "completed result", terminal: &runtime.Outcome{}, stderr: "oops", want: "claude exited with an error: exit status 1: oops"},
		{name: "failure only", terminal: failure, want: "boom"},
		{name: "failure and stderr", terminal: failure, stderr: "oops", want: "boom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := exitError(waitErr, test.terminal, test.stderr)
			if err.Error() != test.want {
				t.Errorf("exitError() = %q, want %q", err, test.want)
			}
			if !errors.Is(err, waitErr) {
				t.Error("exitError() does not wrap the wait error")
			}
		})
	}
}

func TestProcessDriverCancellation(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}'\nwhile :; do :; done\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := NewProcessDriver(ProcessConfig{Executable: executable, Workspace: dir, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := d.Run(ctx, runtime.Turn{Prompt: "hello"}, &recordingSink{})
	if err != context.Canceled {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancellation took too long")
	}
}
