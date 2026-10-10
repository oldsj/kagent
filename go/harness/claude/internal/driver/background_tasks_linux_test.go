//go:build linux

package driver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// The fixture's first eight lines end the first iteration while its background
// command is still live. The rest report the command and Claude's follow-up.
const backgroundFirstIteration = `head -n 8 "$STREAM"
`

func backgroundStream(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../testdata/stream-background-task.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return "STREAM=" + path
}

func TestBackgroundTaskHoldsPostResultGrace(t *testing.T) {
	for _, test := range []struct {
		name, script string
		wantText     string
		minElapsed   time.Duration
	}{
		{
			name:     "live task waits for its follow-up",
			script:   backgroundFirstIteration + "sleep 0.6\ntail -n +9 \"$STREAM\"\n",
			wantText: "waiting for checks checks passed", minElapsed: 600 * time.Millisecond,
		},
		{
			// The follow-up iteration outlasts the remaining grace. It still owes
			// the task's outcome, so the grace stays held until its result.
			name:     "follow-up iteration outlasts grace",
			script:   backgroundFirstIteration + "sleep 0.4\nsed -n 9,12p \"$STREAM\"\nsleep 0.5\ntail -n 1 \"$STREAM\"\n",
			wantText: "waiting for checks checks passed", minElapsed: 900 * time.Millisecond,
		},
		{
			// Without a live task, grace expiry keeps the earlier behavior: the
			// last result is returned and later native work is stopped.
			name:     "no live task keeps grace",
			script:   `grep -v '"task_id":"bash-1"' "$STREAM" | head -n 7` + "\ntrap '' INT\nexec sleep 30\n",
			wantText: "waiting for checks",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver := scriptedDriver(t, test.script, backgroundStream(t))
			driver.config.PostResultGrace = 200 * time.Millisecond
			driver.config.TurnTimeout = 10 * time.Second
			sink := &recordingSink{}
			started := time.Now()
			outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "run checks"}, sink)
			if err != nil || outcome.Failure != nil || outcome.Pending != nil {
				t.Fatalf("Run = %#v, %v", outcome, err)
			}
			if elapsed := time.Since(started); elapsed < test.minElapsed || elapsed > 5*time.Second {
				t.Fatalf("Run took %s, want at least %s", elapsed, test.minElapsed)
			}
			if sink.text.String() != test.wantText {
				t.Fatalf("text = %q, want %q", sink.text.String(), test.wantText)
			}
		})
	}
}

func TestExitWithLiveBackgroundTaskReportsA2AFailure(t *testing.T) {
	runner := scriptedDriver(t, backgroundFirstIteration+"exit 0\n", backgroundStream(t))
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("run checks"))
	message.TaskID, message.ContextID = "background-lost", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
	want := "Claude ended the turn before reporting the outcome of 1 background task"
	var terminal int
	for event, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatal(err)
		}
		if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State.Terminal() {
			terminal++
			if status.Status.State != a2atype.TaskStateFailed || status.Status.Message == nil || status.Status.Message.Parts[0].Text() != want {
				t.Fatalf("terminal event = %#v", status)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal count = %d", terminal)
	}
}

// TestUnreportedBackgroundTaskFailsTurn replays Claude 2.1.260 captures in
// which Claude kills a background task and exits without reporting it: the
// print-mode wind-down kills a shell after the result, and the wait ceiling
// kills an agent and then flushes the result it held. Both must fail rather
// than report the stale result as success.
func TestUnreportedBackgroundTaskFailsTurn(t *testing.T) {
	for _, test := range []struct {
		name, fixture string
		// split is the last line Claude writes before the kill.
		split int
	}{
		{name: "shell wind-down", fixture: "stream-background-wind-down.jsonl", split: 9},
		{name: "agent wait ceiling", fixture: "stream-background-agent-ceiling.jsonl", split: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := filepath.Abs("../../testdata/" + test.fixture)
			if err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("head -n %d \"$STREAM\"\nsleep 0.3\ntail -n +%d \"$STREAM\"\n", test.split, test.split+1)
			driver := scriptedDriver(t, script, "STREAM="+path)
			driver.config.PostResultGrace = 100 * time.Millisecond
			driver.config.TurnTimeout = 10 * time.Second
			outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "run checks"}, &recordingSink{})
			if err != nil || outcome.Pending != nil {
				t.Fatalf("Run = %#v, %v", outcome, err)
			}
			if want := "Claude ended the turn before reporting the outcome of 1 background task"; outcome.Failure == nil || outcome.Failure.Message != want {
				t.Fatalf("outcome = %#v, want failure %q", outcome, want)
			}
		})
	}
}

// TestTaskEndingDuringFinalModelCallHoldsGrace replays a Claude 2.1.260 capture
// in which a background task ends while the final model call is in flight.
// That call's result does not report the task, so the grace must stay held
// until the follow-up iteration's result, even when the follow-up is slow.
func TestTaskEndingDuringFinalModelCallHoldsGrace(t *testing.T) {
	path, err := filepath.Abs("../../testdata/stream-background-mid-call.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	script := "head -n 12 \"$STREAM\"\nsleep 0.5\ntail -n +13 \"$STREAM\"\n"
	driver := scriptedDriver(t, script, "STREAM="+path)
	driver.config.PostResultGrace = 200 * time.Millisecond
	driver.config.TurnTimeout = 10 * time.Second
	sink := &recordingSink{}
	outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "run checks"}, sink)
	if err != nil || outcome.Failure != nil || outcome.Pending != nil {
		t.Fatalf("Run = %#v, %v", outcome, err)
	}
	if !strings.HasSuffix(sink.text.String(), "follow-up after notification") {
		t.Fatalf("text = %q, want the follow-up iteration", sink.text.String())
	}
}

func TestLiveBackgroundTaskStaysWithinExecutionBudget(t *testing.T) {
	dir := t.TempDir()
	activity, pidPath := filepath.Join(dir, "activity"), filepath.Join(dir, "pid")
	// The detached child stands in for the background command Claude is
	// waiting on. Budget expiry must still stop and reap it.
	script := `setsid /bin/sh -c 'trap "" INT; printf "%s\n" "$$" > "$CHILD_PID"; while :; do printf x >> "$ACTIVITY"; sleep 0.02; done' >/dev/null 2>&1 &
while [ ! -s "$ACTIVITY" ]; do sleep 0.01; done
` + backgroundFirstIteration + "trap '' INT\nexec sleep 30\n"
	driver := scriptedDriver(t, script, backgroundStream(t), "ACTIVITY="+activity, "CHILD_PID="+pidPath)
	driver.config.PostResultGrace = 100 * time.Millisecond
	driver.config.TurnTimeout = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	outcome, err := driver.Run(ctx, runtime.Turn{Prompt: "run checks"}, &recordingSink{})
	if !errors.Is(err, errExecutionBudgetExceeded) || outcome.Failure != nil || outcome.Pending != nil {
		t.Fatalf("Run = %#v, %v; want execution budget failure", outcome, err)
	}
	assertChildReaped(t, pidPath)
}

func TestApprovalResumeKeepsGraceHeldForLiveTasks(t *testing.T) {
	session := &processSession{backgroundTasks: 1, postResultBudget: newActiveBudget(time.Hour)}
	session.holdPostResultGrace(true)
	session.holdPostResultGrace(false)
	if session.postResultBudget.done() != nil {
		t.Fatal("approval decision restarted grace while a background task is unreported")
	}
	session.backgroundTasks = 0
	session.holdPostResultGrace(false)
	if session.postResultBudget.done() == nil {
		t.Fatal("grace did not resume after the last background outcome was reported")
	}
}
