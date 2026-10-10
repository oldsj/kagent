//go:build linux

package driver

import (
	"os/exec"
	"reflect"
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

func TestDescendantsExcludePreexistingTreesByIdentity(t *testing.T) {
	processes := map[int]processIdentity{
		1: {pid: 1, parent: 0, started: 1},
		2: {pid: 2, parent: 1, started: 2},
		3: {pid: 3, parent: 2, started: 3},
		4: {pid: 4, parent: 1, started: 4},
		5: {pid: 5, parent: 4, started: 5},
		6: {pid: 6, parent: 99, started: 6},
	}
	for _, test := range []struct {
		name    string
		exclude map[int]uint64
		want    []int
	}{
		{name: "all owned", want: []int{2, 3, 4, 5}},
		{name: "existing sibling and its children", exclude: map[int]uint64{2: 2}, want: []int{4, 5}},
		{name: "reused PID is owned", exclude: map[int]uint64{2: 99}, want: []int{2, 3, 4, 5}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []int
			for _, process := range descendants(processes, 1, test.exclude) {
				got = append(got, process.pid)
			}
			slices.Sort(got)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("descendants = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProcessTreePreservesExistingChildAndRejectsOverlap(t *testing.T) {
	existing := exec.Command("/bin/sleep", "30")
	if err := existing.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = existing.Process.Kill(); _ = existing.Wait() }()
	driver := scriptedDriver(t, waitingStream)
	driver.config.PostResultGrace = 50 * time.Millisecond
	if outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err != nil || outcome.Failure != nil {
		t.Fatalf("Run = %#v, %v", outcome, err)
	}
	if err := unix.Kill(existing.Process.Pid, 0); err != nil {
		t.Fatalf("preexisting sibling was killed: %v", err)
	}
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
	if err := owner.killAndReap(-1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(existing.Process.Pid, 0); err != nil {
		t.Fatalf("preexisting sibling was not preserved: %v", err)
	}
}
