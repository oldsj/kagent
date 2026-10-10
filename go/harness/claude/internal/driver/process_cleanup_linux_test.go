//go:build linux

package driver

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"golang.org/x/sys/unix"
)

type followupReadySink struct {
	recordingSink
	ready chan struct{}
}

func (s *followupReadySink) TextDelta(event runtime.TextDelta) error {
	if event.Text == "review-ready" {
		close(s.ready)
	}
	return s.recordingSink.TextDelta(event)
}

// Port of the reviewer's resource-failure regression. Isolate descriptor
// exhaustion and failed-cleanup ownership retention from the other tests.
func TestFollowupCleanupResourceFailureReturns(t *testing.T) {
	if os.Getenv("KAGENT_REVIEW_RESOURCE_HELPER") != "1" {
		helper := exec.Command(os.Args[0], "-test.run=^TestFollowupCleanupResourceFailureReturns$", "-test.timeout=10s")
		helper.Env = append(os.Environ(), "KAGENT_REVIEW_RESOURCE_HELPER=1")
		output, err := helper.CombinedOutput()
		t.Logf("isolated cleanup probe:\n%s", output)
		if err != nil {
			t.Fatalf("cleanup did not return under descriptor exhaustion: %v", err)
		}
		return
	}

	pidPath := filepath.Join(t.TempDir(), "leader")
	runner := scriptedDriver(t, `printf '%s\n' "$$" > "$REVIEW_PID"
trap '' INT
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"assistant","message":{"id":"ready","content":[{"type":"text","text":"review-ready"}]}}'
exec sleep 30
`, "REVIEW_PID="+pidPath)
	runner.config.TurnTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, runtime.Turn{Prompt: "hello"}, &followupReadySink{ready: ready})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("native did not start")
	}
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
	defer unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	limited := original
	limited.Cur = 3
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
		t.Fatal(err)
	}
	// Demonstrate that cleanup's census really will fail. Do not change source
	// or simulate a successful kill; only the harness's own fd limit changes.
	if _, err := readProcesses(); !errors.Is(err, unix.EMFILE) {
		t.Fatalf("expected descriptor exhaustion, got %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if !errors.Is(err, context.Canceled) || !errors.Is(err, unix.EMFILE) {
			t.Fatalf("cancellation lost its cleanup error: %v", err)
		}
		if err := unix.PidfdSendSignal(fd, 0, nil, 0); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("leader was not reaped through the retained handle: %v", err)
		}
		assertCleanupOwnershipRetained(t)
		t.Logf("cancellation returned with cleanup error: %v", err)
	case <-time.After(time.Second):
		aliveErr := unix.PidfdSendSignal(fd, 0, nil, 0)
		if restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("cancellation hung beyond grace; native alive check=%v; external pidfd rescue released Run: %v", aliveErr, err)
		case <-time.After(2 * time.Second):
			t.Fatal("cleanup still hung after owned native process was killed")
		}
	}
}

// Kernel failures are installed only after the native leader and rescue handles
// exist. Each helper has its own descriptor limits, syscall filter and owner slot.
func TestCleanupFailuresAfterLaunch(t *testing.T) {
	if mode := os.Getenv("KAGENT_CLEANUP_FAILURE_HELPER"); mode != "" {
		runCleanupFailureProbe(t, mode)
		return
	}
	for _, mode := range []string{
		"discovery/budget", "discovery/post-result",
		"pidfd-open/cancellation", "pidfd-open/budget", "pidfd-open/post-result",
	} {
		t.Run(mode, func(t *testing.T) {
			helper := exec.Command(os.Args[0], "-test.run=^TestCleanupFailuresAfterLaunch$", "-test.timeout=10s")
			helper.Env = append(os.Environ(), "KAGENT_CLEANUP_FAILURE_HELPER="+mode)
			output, err := helper.CombinedOutput()
			if err != nil {
				t.Fatalf("isolated cleanup probe: %v\n%s", err, output)
			}
		})
	}
}

func runCleanupFailureProbe(t *testing.T, mode string) {
	t.Helper()
	failure, trigger, _ := strings.Cut(mode, "/")
	dir := t.TempDir()
	leaderPath, childPath := filepath.Join(dir, "leader"), filepath.Join(dir, "child")
	script := `trap '' INT
printf '%s\n' "$$" > "$LEADER_PID"
`
	if failure == "pidfd-open" {
		script += `sleep 30 >/dev/null 2>&1 &
printf '%s\n' "$!" > "$CHILD_PID"
`
	}
	script += `printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}'
`
	if trigger == "post-result" {
		script += `printf '%s\n' '{"type":"result","subtype":"success"}'
`
	}
	script += `printf '%s\n' '{"type":"assistant","message":{"id":"ready","content":[{"type":"text","text":"review-ready"}]}}'
exec sleep 30
`
	runner := scriptedDriver(t, script, "LEADER_PID="+leaderPath, "CHILD_PID="+childPath)
	runner.config.TurnTimeout = 5 * time.Second
	if trigger == "budget" {
		runner.config.TurnTimeout = 500 * time.Millisecond
	}
	if trigger == "post-result" {
		runner.config.PostResultGrace = 500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, done := make(chan struct{}), make(chan runResult, 1)
	go func() {
		outcome, err := runner.Run(ctx, runtime.Turn{Prompt: "hello"}, &followupReadySink{ready: ready})
		done <- runResult{outcome: outcome, err: err}
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("native did not start")
	}
	leaderFD := rescuePIDFile(t, leaderPath)
	childFD := -1
	if failure == "pidfd-open" {
		childFD = rescuePIDFile(t, childPath)
	}
	finished := false
	defer func() {
		_ = unix.PidfdSendSignal(leaderFD, unix.SIGKILL, nil, 0)
		if !finished {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("native did not exit after rescue")
			}
		}
		_ = unix.Close(leaderFD)
		if childFD >= 0 {
			_ = unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0)
			if err := unix.Waitid(unix.P_PIDFD, childFD, nil, unix.WEXITED, nil); err != nil {
				t.Errorf("reap rescued adopted child: %v", err)
			}
			_ = unix.Close(childFD)
		}
	}()
	if failure == "discovery" {
		var original unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
				t.Errorf("restore descriptor limit: %v", err)
			}
		}()
		limited := original
		limited.Cur = 3
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
			t.Fatal(err)
		}
		if _, err := readProcesses(); !errors.Is(err, unix.EMFILE) {
			t.Fatalf("discovery failure was not installed: %v", err)
		}
	} else {
		denyPidfdOpen(t)
		processes, err := readProcesses()
		if err != nil {
			t.Fatalf("pidfd-open filter broke discovery: %v", err)
		}
		if _, err := openProcess(processes[readPIDFile(t, leaderPath)]); !errors.Is(err, unix.EMFILE) {
			t.Fatalf("pidfd-open failure was not installed: %v", err)
		}
	}
	if trigger == "cancellation" {
		cancel()
	}
	select {
	case result := <-done:
		finished = true
		if !errors.Is(result.err, unix.EMFILE) {
			t.Fatalf("cleanup error was lost: %v", result.err)
		}
		switch trigger {
		case "cancellation":
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", result.err)
			}
		case "budget":
			if !errors.Is(result.err, errExecutionBudgetExceeded) {
				t.Fatalf("execution budget failure was lost: %v", result.err)
			}
		case "post-result":
			if result.outcome.Failure != nil || errors.Is(result.err, errExecutionBudgetExceeded) {
				t.Fatalf("post-result outcome = %#v, %v", result.outcome, result.err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not return after cancellation or budget expiry")
	}
	if err := unix.PidfdSendSignal(leaderFD, 0, nil, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("native leader was not reaped: %v", err)
	}
	if childFD >= 0 {
		if err := unix.PidfdSendSignal(childFD, 0, nil, 0); err != nil {
			t.Fatalf("probe did not retain a child whose pidfd could not be opened: %v", err)
		}
	}
	assertCleanupOwnershipRetained(t)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid native PID: %q", raw)
	}
	return pid
}

func rescuePIDFile(t *testing.T, path string) int {
	t.Helper()
	fd, err := unix.PidfdOpen(readPIDFile(t, path), 0)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func assertCleanupOwnershipRetained(t *testing.T) {
	t.Helper()
	if owner, err := newProcessTree(); err == nil {
		owner.release()
		t.Fatal("failed cleanup released native ownership")
	} else if !strings.Contains(err.Error(), "another Claude process tree") {
		t.Fatalf("failed cleanup did not retain its ownership slot: %v", err)
	}
}

// Deny new pidfds across every harness thread, while leaving /proc and existing
// pidfd signals available. The already-running native tree is unaffected.
func denyPidfdOpen(t *testing.T) {
	t.Helper()
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // seccomp_data.nr
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(unix.SYS_PIDFD_OPEN), Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EMFILE)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	synced, _, err := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	if err != 0 || synced != 0 {
		t.Fatalf("install pidfd-open failure filter: errno=%v, unsynced thread=%d", err, synced)
	}
}

func TestCleanupFailedLeaderSignalBoundsWait(t *testing.T) {
	if os.Getenv("KAGENT_CLEANUP_WAIT_HELPER") != "1" {
		helper := exec.Command(os.Args[0], "-test.run=^TestCleanupFailedLeaderSignalBoundsWait$", "-test.timeout=10s")
		helper.Env = append(os.Environ(), "KAGENT_CLEANUP_WAIT_HELPER=1")
		if output, err := helper.CombinedOutput(); err != nil {
			t.Fatalf("isolated leader-wait probe: %v\n%s", err, output)
		}
		return
	}
	owner, err := newProcessTree()
	if err != nil {
		t.Fatal(err)
	}
	// No real child is needed to model an unsuccessful signal and a wait that
	// never completes. EBADF must be returned without releasing ownership.
	owner.leader = &processHandle{fd: -1}
	items := make(chan parseItem)
	close(items)
	session := &processSession{
		owner: owner, items: items, stopEmit: make(chan struct{}), wait: make(chan error),
		stdin: nopWriteCloser{Writer: io.Discard}, stdout: io.NopCloser(strings.NewReader("")),
	}
	runner := NewProcessDriver(ProcessConfig{InterruptGrace: 20 * time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.stopSession(session) }()
	select {
	case err := <-done:
		if !errors.Is(err, unix.EBADF) || !strings.Contains(err.Error(), "Claude leader cleanup did not finish") {
			t.Fatalf("failed leader signal error = %v", err)
		}
		if owner.leader == nil {
			t.Fatal("unconfirmed exit discarded the retained leader handle")
		}
		assertCleanupOwnershipRetained(t)
		if repeated := runner.stopSession(session); repeated != err {
			t.Fatalf("repeated cleanup lost the original failure: %v", repeated)
		}
	case <-time.After(time.Second):
		t.Fatal("failed leader signal caused an unbounded wait")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestProcessDriverLaunchFailureReleasesOwnership(t *testing.T) {
	runner := scriptedDriver(t, "")
	// Force an exec failure after fork, exercising Go's launch-pidfd cleanup.
	if err := os.WriteFile(runner.config.Executable, []byte("invalid executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); !errors.Is(err, unix.ENOEXEC) {
		t.Fatalf("launch error = %v, want ENOEXEC", err)
	}
	next := scriptedDriver(t, `printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}'`)
	if outcome, err := next.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err != nil || outcome.Failure != nil {
		t.Fatalf("Run after launch failure = %#v, %v", outcome, err)
	}
}
