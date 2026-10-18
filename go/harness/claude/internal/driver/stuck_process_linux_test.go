//go:build linux

package driver

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const (
	detachedDescendantEnv = "KAGENT_TEST_DETACHED_DESCENDANT"
	stuckSessionID        = "11111111-1111-4111-8111-111111111111"
)

// TestDetachedDescendantHelper is not a test. The fake Claude below starts the
// test binary through it to get a descendant that leaves Claude's process group
// and keeps the inherited stdout and stderr open after the group is killed.
func TestDetachedDescendantHelper(t *testing.T) {
	pidPath := os.Getenv(detachedDescendantEnv)
	if pidPath == "" {
		t.Skip("helper process only")
	}
	if err := syscall.Setpgid(0, 0); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

// fakeStuckClaude is a Claude process that starts two tool calls and then
// never finishes them. It ignores interrupts, and a descendant outside its
// process group holds stdout and stderr open after the group is killed. The
// first invocation is stuck; later invocations complete normally.
func fakeStuckClaude(t *testing.T) (ProcessConfig, string) {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	pidPath := filepath.Join(dir, "descendant.pid")
	init := `{"type":"system","subtype":"init","session_id":"` + stuckSessionID + `"}`
	toolUse := func(id string) string {
		return `{"type":"assistant","session_id":"` + stuckSessionID + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"WebSearch","input":{"query":"q"}}]}}`
	}
	result := `{"type":"result","subtype":"success","session_id":"` + stuckSessionID + `"}`
	script := `#!/bin/sh
if [ -e "$STUCK_MARKER" ]; then
  IFS= read -r line
  printf '%s\n' '` + init + `' '` + result + `'
  exit 0
fi
: > "$STUCK_MARKER"
trap '' INT TERM
KAGENT_TEST_DETACHED_DESCENDANT="$DESCENDANT_PID" "$TEST_BINARY" -test.run='^TestDetachedDescendantHelper$' &
printf '%s\n' '` + init + `' '` + toolUse("toolu_1") + `' '` + toolUse("toolu_2") + `'
exec sleep 60
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return ProcessConfig{
		Executable: executable, Workspace: dir,
		Environment: []string{
			"STUCK_MARKER=" + filepath.Join(dir, "stuck"), "DESCENDANT_PID=" + pidPath, "TEST_BINARY=" + testBinary,
		},
		MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 100 * time.Millisecond,
	}, pidPath
}

func TestProcessDriverStopIsBoundedWhenDescendantHoldsOutput(t *testing.T) {
	config, pidPath := fakeStuckClaude(t)
	d := NewProcessDriver(config)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &recordingSink{}
	result := make(chan error, 1)
	go func() {
		_, err := d.Run(ctx, runtime.Turn{Prompt: "search"}, sink)
		result <- err
	}()
	waitForFile(t, pidPath)

	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation while a descendant held stdout")
	}
}

func TestCancelSettlesStuckClaudeTurnAndFreesExecutor(t *testing.T) {
	config, pidPath := fakeStuckClaude(t)
	executor, err := runtimea2a.New(NewProcessDriver(config), &memoryContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan []a2atype.Event, 1)
	go func() {
		events, _ := collectEvents(executor.Execute(context.Background(), userRequest("task-1", "search")))
		first <- events
	}()
	waitForFile(t, pidPath)

	// The SDK cancels with a context detached from the caller, so the bound
	// must come from the runtime rather than from this deadline.
	cancelCtx, cancelDone := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDone()
	started := time.Now()
	events, errs := collectEvents(executor.Cancel(cancelCtx, userRequest("task-1", "")))
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Cancel() took %s while a descendant held Claude's output", elapsed)
	}
	if len(errs) != 0 || len(events) != 1 {
		t.Fatalf("Cancel() events/errors = %#v/%v", events, errs)
	}
	if status, ok := events[0].(*a2atype.TaskStatusUpdateEvent); !ok || status.Status.State != a2atype.TaskStateCanceled {
		t.Fatalf("Cancel() event = %#v, want canceled", events[0])
	}
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled execution did not return")
	}

	// The Actor accepts its next turn on the same native conversation.
	events, errs = collectEvents(executor.Execute(t.Context(), userRequest("task-2", "next")))
	if len(errs) != 0 || len(events) == 0 {
		t.Fatalf("next Execute() events/errors = %#v/%v", events, errs)
	}
	last, ok := events[len(events)-1].(*a2atype.TaskStatusUpdateEvent)
	if !ok || last.Status.State != a2atype.TaskStateCompleted {
		t.Fatalf("next Execute() final event = %#v, want completed", events[len(events)-1])
	}
}

type memoryContinuation struct{ id string }

func (m *memoryContinuation) Load() (string, bool, error) { return m.id, m.id != "", nil }
func (m *memoryContinuation) Bind(id string) error        { m.id = id; return nil }

func userRequest(taskID a2atype.TaskID, text string) *a2asrv.ExecutorContext {
	request := &a2asrv.ExecutorContext{TaskID: taskID, ContextID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	if text != "" {
		request.Message = a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
		request.Message.TaskID, request.Message.ContextID = taskID, request.ContextID
	}
	return request
}

func collectEvents(seq iter.Seq2[a2atype.Event, error]) ([]a2atype.Event, []error) {
	var events []a2atype.Event
	var errs []error
	for event, err := range seq {
		if event != nil {
			events = append(events, event)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return events, errs
}

func TestSuccessfulExitToleratesDescendantHoldingStderr(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	// The descendant keeps the inherited stderr open after Claude exits 0, so
	// Wait ends with ErrWaitDelay rather than success.
	runner := scriptedDriver(t, `sh -c 'printf "%s\n" "$$" > "$CHILD_PID"; exec sleep 60' >/dev/null &
while [ ! -s "$CHILD_PID" ]; do sleep 0.01; done
printf '%s\n' '{"type":"system","subtype":"init","session_id":"`+stuckSessionID+`"}' '{"type":"result","subtype":"success"}'
`, "CHILD_PID="+pidPath)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := runner.Run(ctx, runtime.Turn{Prompt: "hello"}, &recordingSink{})
	if err != nil || outcome.Failure != nil || outcome.Pending != nil {
		t.Fatalf("Run = %#v, %v; want success", outcome, err)
	}
	assertChildReaped(t, pidPath)
}
