//go:build linux

package driver

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"golang.org/x/sys/unix"
)

// Port of the reviewer's orphan probe. Admission now rejects the existing
// tree, before the native script can run. Its later adopted child also blocks
// admission and remains alive; a one-time exclusion census cannot protect it.
func TestReReviewExcludedTreeKeepsNewOrphan(t *testing.T) {
	dir := t.TempDir()
	ready, trigger, childPID := filepath.Join(dir, "ready"), filepath.Join(dir, "trigger"), filepath.Join(dir, "pid")
	nativeStarted := filepath.Join(dir, "native-started")
	preexisting := exec.Command("/bin/sh", "-c", `printf x > "$READY"
while [ ! -e "$TRIGGER" ]; do sleep 0.01; done
setsid /bin/sh -c 'trap "" INT; printf "%s\n" "$$" > "$CHILD_PID"; exec sleep 30' >/dev/null 2>&1 &
while [ ! -s "$CHILD_PID" ]; do sleep 0.01; done
`)
	preexisting.Env = []string{"PATH=/usr/bin:/bin", "READY=" + ready, "TRIGGER=" + trigger, "CHILD_PID=" + childPID}
	if err := preexisting.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = preexisting.Process.Kill(); _ = preexisting.Wait() }()
	waitForFile(t, ready)
	runner := scriptedDriver(t, `printf x > "$NATIVE_STARTED"`, "NATIVE_STARTED="+nativeStarted)
	if _, err := runner.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err == nil || !strings.Contains(err.Error(), "pre-existing descendant trees") {
		t.Fatalf("Run with existing tree error = %v", err)
	}
	if _, err := os.Stat(nativeStarted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native script ran despite failed ownership admission: %v", err)
	}
	// Fork after the rejected launch, reproducing the later orphan independently
	// of Claude. Admission has enabled subreaping but owns neither process.
	if err := os.WriteFile(trigger, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, childPID)
	raw, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid child PID: %q", raw)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		// Block until the test-owned adopted orphan is reaped so later tests
		// can acquire the empty descendant boundary.
		if err := unix.Waitid(unix.P_PIDFD, fd, nil, unix.WEXITED, nil); err != nil {
			t.Errorf("reap test orphan: %v", err)
		}
		_ = unix.Close(fd)
	}()
	if err := preexisting.Wait(); err != nil {
		t.Fatal(err)
	}
	identity, err := readProcess(pid)
	if err != nil || identity.parent != os.Getpid() {
		t.Fatalf("orphan was not adopted: %#v, %v", identity, err)
	}
	if _, err := runner.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err == nil || !strings.Contains(err.Error(), "pre-existing descendant trees") {
		t.Fatalf("Run with adopted existing orphan error = %v", err)
	}
	if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		t.Fatalf("admission killed an orphan from a pre-existing tree: %v", err)
	}
}

func TestReReviewStaleBirthIdentityIsSkipped(t *testing.T) {
	peer := exec.Command("/bin/sleep", "30")
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
	identity, err := readProcess(peer.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.started++
	if err := signalProcess(identity, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := peer.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stale birth identity was signaled: %v", err)
	}
}

// Model PID reuse after native leader reaping with a real test-owned peer
// group. An isolated test harness keeps the peer outside its descendants,
// allowing admission under the stricter dedicated-process contract.
func TestReReviewCleanupRejectsReusedLeaderGroup(t *testing.T) {
	if value, helper := strings.CutPrefix(os.Args[len(os.Args)-1], "peer-pid="); helper {
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := newProcessTree()
		if err != nil {
			t.Fatal(err)
		}
		defer owner.release()
		staleLeader, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		defer staleLeader.Release()
		stdin, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		items := make(chan parseItem)
		close(items)
		wait := make(chan error)
		close(wait)
		session := &processSession{
			command: &exec.Cmd{Process: staleLeader}, owner: owner,
			items: items, stopEmit: make(chan struct{}), wait: wait,
			stdin: stdin, stdout: io.NopCloser(strings.NewReader("")),
		}
		driver := NewProcessDriver(ProcessConfig{InterruptGrace: 20 * time.Millisecond})
		if err := driver.stopSession(session); err != nil {
			t.Fatal(err)
		}
		return
	}
	peer := exec.Command("/bin/sleep", "30")
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Wait() }()
	defer func() {
		_ = peer.Process.Kill()
		select {
		case <-peerDone:
		case <-time.After(time.Second):
			t.Error("peer cleanup did not finish")
		}
	}()
	helper := exec.Command(os.Args[0], "-test.run=^TestReReviewCleanupRejectsReusedLeaderGroup$", "--", "peer-pid="+strconv.Itoa(peer.Process.Pid))
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("isolated cleanup: %v\n%s", err, output)
	}
	select {
	case err := <-peerDone:
		peerDone <- err
		t.Fatalf("stale group PID killed the unrelated peer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
