//go:build linux

package driver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/harness/runtime/service"
)

// The test binary stands in for the harness binary when a test starts a
// workspace service.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == service.SupervisorArg {
		os.Exit(service.Supervise(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestProcessTreeLeavesWorkspaceServiceAlone(t *testing.T) {
	t.Run("Given a running workspace service whose daemon outlived its leader, when a Claude turn runs and its tree is cleaned up, assert that the service keeps running", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "daemon.pid")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- service.Run(ctx, service.Spec{
				Command: []string{"/bin/sh", "-c", "/bin/sleep 60 </dev/null >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"},
				WorkDir: dir, Dir: dir, Environment: os.Environ(), Executable: os.Args[0],
			}, slog.Default())
		}()
		t.Cleanup(func() { cancel(); <-done })
		supervisor := waitForServicePID(t, filepath.Join(dir, service.StatusFile))
		daemon := waitForFilePID(t, pidFile)
		driver := scriptedDriver(t, waitingStream)
		driver.config.PostResultGrace = 50 * time.Millisecond

		outcome, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{})

		if err != nil || outcome.Failure != nil {
			t.Fatalf("Run = %#v, %v; want a turn admitted beside the service", outcome, err)
		}
		for _, pid := range []int{supervisor, daemon} {
			if err := unix.Kill(pid, 0); err != nil {
				t.Fatalf("workspace service process %d did not survive the turn: %v", pid, err)
			}
		}
	})
}

func waitForServicePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status service.Status
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &status) == nil && status.State == "running" {
			return status.PID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the workspace service never ran")
	return 0
}

func waitForFilePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon never recorded its PID")
	return 0
}
