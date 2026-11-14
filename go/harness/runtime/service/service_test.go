package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for the harness binary: Run re-executes it in
// supervisor mode.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SupervisorArg {
		os.Exit(Supervise(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestWorkspaceService(t *testing.T) {
	t.Run("Given a declared command, when the service exits, assert that its output is logged and its exit status recorded", func(t *testing.T) {
		dir := t.TempDir()

		err := Run(t.Context(), spec(dir, "/bin/sh", "-c", "pwd; echo from-service; echo to-stderr >&2; exit 3"), slog.Default())

		if err == nil {
			t.Fatal("Run() error = nil, want the exit status")
		}
		status := readStatus(t, dir)
		if status.State != "exited" || status.ExitCode == nil || *status.ExitCode != 3 || status.PID == 0 {
			t.Fatalf("status = %+v, want exited with code 3", status)
		}
		log := readFile(t, filepath.Join(dir, LogFile))
		for _, want := range []string{"from-service", "to-stderr", spec(dir).WorkDir} {
			if !strings.Contains(log, want) {
				t.Fatalf("log = %q, want %q", log, want)
			}
		}
	})

	t.Run("Given a running service, when the harness takes a census, assert that it lists the supervisor until the service stops", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)

		go func() { done <- Run(ctx, spec(dir, "/bin/sleep", "60"), slog.Default()) }()
		running := waitForState(t, dir, "running")

		if supervisors := census(t); len(supervisors) != 1 || supervisors[0].PID != running.PID {
			t.Fatalf("census = %+v, want the supervisor %d", supervisors, running.PID)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(stopGrace + 10*time.Second):
			t.Fatal("Run did not return after its context ended")
		}
		if supervisors := census(t); len(supervisors) != 0 {
			t.Fatalf("census after stop = %+v, want none", supervisors)
		}
		if status := readStatus(t, dir); status.State != "exited" {
			t.Fatalf("status after stop = %+v, want exited", status)
		}
	})

	t.Run("Given a command that is not on PATH, when the service starts, assert that the failure is logged and recorded", func(t *testing.T) {
		dir := t.TempDir()

		err := Run(t.Context(), spec(dir, "kagent-no-such-command"), slog.Default())

		if err == nil {
			t.Fatal("Run() error = nil, want a failure")
		}
		if status := readStatus(t, dir); status.State != "exited" || status.ExitCode == nil || *status.ExitCode == 0 {
			t.Fatalf("status = %+v, want a nonzero exit", status)
		}
		if log := readFile(t, filepath.Join(dir, LogFile)); !strings.Contains(log, "kagent-no-such-command") {
			t.Fatalf("log = %q, want the missing command named", log)
		}
	})
}

func TestRotatingLog(t *testing.T) {
	t.Run("Given a service's start header, when output rotates the log past both files, assert that the newest file still opens with the header", func(t *testing.T) {
		dir := t.TempDir()
		log, err := openRotatingLog(dir, 64)
		if err != nil {
			t.Fatal(err)
		}
		log.header = "== started\n"
		if _, err := log.Write([]byte(log.header)); err != nil {
			t.Fatal(err)
		}

		for range 20 {
			if _, err := log.Write([]byte("0123456789abcdef\n")); err != nil {
				t.Fatal(err)
			}
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}

		for _, name := range []string{LogFile, RotatedLogFile} {
			if got := readFile(t, filepath.Join(dir, name)); !strings.HasPrefix(got, "== started\n") {
				t.Fatalf("%s = %q, want it to open with the start header", name, got)
			}
		}
	})

	t.Run("Given a log at its cap, when more output arrives, assert that the log rotates once and keeps two files", func(t *testing.T) {
		dir := t.TempDir()
		log, err := openRotatingLog(dir, 10)
		if err != nil {
			t.Fatal(err)
		}

		for _, line := range []string{"first\n", "second\n", "third\n"} {
			if _, err := log.Write([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}

		if got := readFile(t, filepath.Join(dir, RotatedLogFile)); got != "second\n" {
			t.Fatalf("rotated log = %q, want the previous file", got)
		}
		if got := readFile(t, filepath.Join(dir, LogFile)); got != "third\n" {
			t.Fatalf("log = %q, want the newest output", got)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("log directory = %v, %v; want two files", entries, err)
		}
	})
}

func spec(dir string, command ...string) Spec {
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		panic(err)
	}
	// Resolve the working directory as the kernel reports it (macOS
	// temporary directories are behind a symlink).
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	return Spec{Command: command, WorkDir: work, Dir: dir, Environment: os.Environ(), Executable: os.Args[0]}
}

func census(t *testing.T) []Identity {
	t.Helper()
	var supervisors []Identity
	if err := Census(func(live []Identity) error { supervisors = live; return nil }); err != nil {
		t.Fatal(err)
	}
	return supervisors
}

func waitForState(t *testing.T, dir, state string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, StatusFile)); err == nil {
			var status Status
			if json.Unmarshal(data, &status) == nil && status.State == state {
				return status
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status never reached %s", state)
	return Status{}
}

func readStatus(t *testing.T, dir string) Status {
	t.Helper()
	var status Status
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, StatusFile))), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
