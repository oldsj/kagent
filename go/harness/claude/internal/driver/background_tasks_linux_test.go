//go:build linux

package driver

import (
	"context"
	"errors"
	"path/filepath"
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
	want := "Claude ended the turn with 1 background task still running; their results were lost"
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
		t.Fatal("approval decision restarted grace while a background task is live")
	}
	session.backgroundTasks = 0
	session.holdPostResultGrace(false)
	if session.postResultBudget.done() == nil {
		t.Fatal("grace did not resume after the last background task ended")
	}
}
