//go:build linux

package driver

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"golang.org/x/sys/unix"
)

func TestParseProcessIdentity(t *testing.T) {
	fields := strings.Fields("S 12 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 345")
	stat := "23 (name with ) parentheses) " + strings.Join(fields, " ")
	process, err := parseProcessIdentity(23, stat)
	if err != nil || process != (processIdentity{pid: 23, parent: 12, started: 345}) {
		t.Fatalf("identity = %#v, %v", process, err)
	}
	for _, invalid := range []string{"missing command", "23 (cmd) S 12", strings.Replace(stat, "S 12", "S bad", 1), strings.Replace(stat, "345", "bad", 1)} {
		if _, err := parseProcessIdentity(23, invalid); err == nil {
			t.Fatalf("accepted invalid stat %q", invalid)
		}
	}
}

func TestDescendants(t *testing.T) {
	processes := map[int]processIdentity{
		1: {pid: 1, parent: 0, started: 1},
		2: {pid: 2, parent: 1, started: 2},
		3: {pid: 3, parent: 2, started: 3},
		4: {pid: 4, parent: 1, started: 4},
		5: {pid: 5, parent: 4, started: 5},
		6: {pid: 6, parent: 99, started: 6},
	}
	for _, test := range []struct {
		name string
		root int
		want []int
	}{
		{name: "harness tree", root: 1, want: []int{2, 3, 4, 5}},
		{name: "native subtree", root: 2, want: []int{3}},
		{name: "no descendants", root: 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []int
			for _, process := range descendants(processes, test.root) {
				got = append(got, process.pid)
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("descendants = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProcessTreeRejectsExistingChildAndOverlap(t *testing.T) {
	existing := exec.Command("/bin/sleep", "30")
	if err := existing.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = existing.Process.Kill(); _ = existing.Wait() }()
	driver := scriptedDriver(t, waitingStream)
	driver.config.PostResultGrace = 50 * time.Millisecond
	if _, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err == nil || !strings.Contains(err.Error(), "pre-existing descendant trees") {
		t.Fatalf("Run error = %v", err)
	}
	if err := unix.Kill(existing.Process.Pid, 0); err != nil {
		t.Fatalf("preexisting sibling was killed: %v", err)
	}
	if err := existing.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = existing.Wait()
	owner, err := newProcessTree()
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()
	if _, err := newProcessTree(); err == nil {
		t.Fatal("concurrent tree acquired ownership")
	}
	if _, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err == nil || !strings.Contains(err.Error(), "another Claude process tree") {
		t.Fatalf("overlapping Run error = %v", err)
	}
	if err := owner.interrupt(); err != nil {
		t.Fatal(err)
	}
	if err := owner.killAndReap(); err != nil {
		t.Fatal(err)
	}
	owner.release()
	if outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err != nil || outcome.Failure != nil {
		t.Fatalf("Run after ownership release = %#v, %v", outcome, err)
	}
}

func TestProcessHandleStaysBoundAfterReaping(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	identity, err := readProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := openProcess(identity)
	if err != nil || handle == nil {
		t.Fatalf("openProcess = %#v, %v", handle, err)
	}
	defer unix.Close(handle.fd)
	if err := handle.signal(unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if err := unix.PidfdSendSignal(handle.fd, 0, nil, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("reaped handle signal = %v, want ESRCH", err)
	}
	peer := exec.Command("/bin/sleep", "30")
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
	// Model a retained PID naming a replacement without forcing PID wraparound.
	handle.pid = peer.Process.Pid
	if err := handle.signal(unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := peer.Process.Signal(unix.Signal(0)); err != nil {
		t.Fatalf("reaped handle signaled a replacement peer: %v", err)
	}
	identity, err = readProcess(peer.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.parent = os.Getpid() + 1
	if handle, err := openProcess(identity); err != nil || handle != nil {
		t.Fatalf("changed ancestry handle = %#v, %v", handle, err)
	}
}
