//go:build linux

package service

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkspaceServiceKeepsDaemons(t *testing.T) {
	t.Run("Given a command that daemonizes, when its leader exits, assert that the supervisor keeps the daemon and stops it when the service stops", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := dir + "/daemon.pid"
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() {
			done <- Run(ctx, spec(dir, "/bin/sh", "-c", "setsid /bin/sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile+"; exit 0"), slog.Default())
		}()
		running := waitForState(t, dir, "running")
		daemon := waitForPID(t, pidFile)
		waitForParent(t, daemon, running.PID)

		select {
		case err := <-done:
			t.Fatalf("Run returned while the daemon runs: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
		select {
		case <-done:
		case <-time.After(stopGrace + 10*time.Second):
			t.Fatal("Run did not return after its context ended")
		}
		if err := unix.Kill(daemon, 0); err == nil {
			t.Fatalf("daemon %d survived the service stop", daemon)
		}
	})
}

func waitForPID(t *testing.T, path string) int {
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

// waitForParent waits until pid's parent is parent: the daemon's shell exits
// right after starting it, and the subreaper adopts it.
func waitForParent(t *testing.T, pid, parent int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := readProcess(pid); err == nil && p.parent == parent {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, err := readProcess(pid)
	t.Fatalf("daemon %d parent = %+v, %v; want the supervisor %d", pid, p, err, parent)
}
