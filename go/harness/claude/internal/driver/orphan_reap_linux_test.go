//go:build linux

package driver

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"golang.org/x/sys/unix"
)

const completedTurnScript = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}'`

func TestParseProcessStatus(t *testing.T) {
	stat := "23 (name with ) parentheses) Z 12 77 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 345"
	process, state, group, err := parseProcessStatus(23, stat)
	if err != nil || process != (processIdentity{pid: 23, parent: 12, started: 345}) || state != "Z" || group != 77 {
		t.Fatalf("status = %#v %q %d, %v", process, state, group, err)
	}
	if _, _, _, err := parseProcessStatus(23, strings.Replace(stat, " 77 ", " bad ", 1)); err == nil {
		t.Fatal("accepted invalid process group")
	}
}

// waitForZombieChild waits until pid has exited as a direct, unreaped child.
func waitForZombieChild(t *testing.T, pid int) processIdentity {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err == nil {
			process, state, _, err := parseProcessStatus(pid, string(data))
			if err != nil {
				t.Fatal(err)
			}
			if state == "Z" && process.parent == os.Getpid() {
				return process
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not become a zombie child", pid)
	return processIdentity{}
}

// Reproduces an agent's `git commit` between turns: Git detaches maintenance
// with setsid, the detaching parent exits, and the subreaping harness adopts
// the helper, which then exits unreaped. The next turn must still launch.
func TestTurnAfterOrphanedZombiePassesCensus(t *testing.T) {
	runner := scriptedDriver(t, completedTurnScript)
	if outcome, err := runner.Run(t.Context(), runtime.Turn{Prompt: "first"}, &recordingSink{}); err != nil || outcome.Failure != nil {
		t.Fatalf("first turn = %#v, %v", outcome, err)
	}
	pidPath := filepath.Join(t.TempDir(), "orphan")
	detach := exec.Command("/bin/sh", "-c", `setsid /bin/sh -c 'printf "%s\n" "$$" > "$ORPHAN_PID"; sleep 0.2' >/dev/null 2>&1 &
while [ ! -s "$ORPHAN_PID" ]; do sleep 0.01; done`)
	detach.Env = []string{"PATH=/usr/bin:/bin", "ORPHAN_PID=" + pidPath}
	if output, err := detach.CombinedOutput(); err != nil {
		t.Fatalf("detach helper: %v\n%s", err, output)
	}
	pid := readPIDFile(t, pidPath)
	orphan := waitForZombieChild(t, pid)
	if outcome, err := runner.Run(t.Context(), runtime.Turn{Prompt: "second"}, &recordingSink{}); err != nil || outcome.Failure != nil {
		t.Fatalf("turn after orphaned zombie = %#v, %v", outcome, err)
	}
	if current, err := readProcess(pid); err == nil && current == orphan {
		t.Fatalf("orphaned zombie %d was not reaped", pid)
	}
}

// A zombie in the harness's own process group may be an exec.Cmd child whose
// Wait has not run yet. Admission must not steal its exit status; it reports
// the census rejection as a terminal failure instead.
func TestOrphanReapSparesHarnessGroupZombie(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", "exit 7")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waitForZombieChild(t, child.Process.Pid)
	_, err := newProcessTree()
	var terminal *runtime.TerminalFailure
	if !errors.As(err, &terminal) || !strings.Contains(terminal.PublicMessage(), "pre-existing descendant trees") {
		t.Fatalf("admission error = %v, want terminal census rejection", err)
	}
	var exitErr *exec.ExitError
	if err := child.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("exec.Cmd wait = %v, want its own exit status 7", err)
	}
	if _, err := unix.Getpgid(child.Process.Pid); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("child was not reaped by its own Wait: %v", err)
	}
	owner, err := newProcessTree()
	if err != nil {
		t.Fatal(err)
	}
	owner.release()
}
