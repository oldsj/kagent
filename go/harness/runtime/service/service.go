// Package service runs a Session's declared workspace service, a long-running
// process such as a development server, beside the agent's native process tree.
//
// The harness stays the only process owner in the Actor. It starts one
// supervisor, which is the harness binary re-executed in supervisor mode. The
// supervisor makes itself a child subreaper, so every process the service
// starts, including one that daemonizes, stays in the supervisor's subtree
// rather than being adopted by the harness. Process supervisors that claim the
// harness's descendants (the Claude process driver) take a [Census] and leave
// that subtree alone.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// SupervisorArg is argv[1] of the harness binary in supervisor mode. Each
// harness main hands such an invocation to [Supervise] before anything else.
const SupervisorArg = "__kagent-workspace-service"

// File names inside the service directory. The agent reads them; the guidance
// in [Guidance] names them.
const (
	StatusFile = "status.json"
	LogFile    = "log"
	// RotatedLogFile holds the previous LogFile after rotation.
	RotatedLogFile = "log.1"
)

// maxLogBytes caps LogFile. With RotatedLogFile the service's output takes at
// most twice this on /data.
const maxLogBytes = 8 << 20

// stopGrace is how long the supervisor waits after SIGTERM before it kills the
// service's remaining processes. The harness waits a little longer for the
// supervisor itself.
const stopGrace = 10 * time.Second

// Guidance tells the agent about a declared service. It is worded so that it
// holds whether or not the Session declares one.
func Guidance(dir string) string {
	status := filepath.Join(dir, StatusFile)
	return "# Workspace service\n\n" +
		"If `" + status + "` exists, this Session declares a workspace service, such as a development server. " +
		"The runtime starts its command in the workspace at every start and resume, so it is already running while `state` is `running`. " +
		"Do not start a second copy of it or bind its ports. Its output is in `" + filepath.Join(dir, LogFile) + "` (older output in `" + RotatedLogFile + "`).\n" +
		"To restart it, for example after a migration: run `kill -TERM <pid>` with the `pid` from the status file, wait until `state` is `exited`, " +
		"then run its `command` yourself in the workspace. Your copy runs only until the runtime next stops; the runtime starts the declared service again at its next start.\n"
}

// Identity is a live supervisor: its PID and the start time the kernel
// reports for it, so a reused PID never matches.
type Identity struct {
	PID     int
	Started uint64
}

var (
	censusMu sync.Mutex
	roots    = map[int]Identity{}
)

// Census runs fn with the supervisors that are running, and holds off any
// supervisor start or exit until fn returns. A caller that scans /proc for the
// harness's descendants does the whole scan inside fn, so a service started
// mid-scan is never mistaken for one of its own processes.
func Census(fn func([]Identity) error) error {
	censusMu.Lock()
	defer censusMu.Unlock()
	live := make([]Identity, 0, len(roots))
	for _, identity := range roots {
		live = append(live, identity)
	}
	return fn(live)
}

// Status is the service's state as the agent sees it in StatusFile.
type Status struct {
	Command []string `json:"command"`
	// State is starting, running, exited or failed.
	State string `json:"state"`
	// PID is the supervisor's. Signaling it with SIGTERM stops the service.
	PID       int        `json:"pid,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	ExitedAt  *time.Time `json:"exited_at,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// WriteStatus replaces dir's StatusFile atomically.
func WriteStatus(dir string, status Status) error {
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, "."+StatusFile+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filepath.Join(dir, StatusFile))
}

// RemoveStatus removes a StatusFile left by an earlier start, so a Session
// that declares no service shows none.
func RemoveStatus(dir string) error {
	err := os.Remove(filepath.Join(dir, StatusFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Spec is one start of the service.
type Spec struct {
	// Command is the declared argv. Its first element is resolved through the
	// PATH in Environment.
	Command []string
	// WorkDir is the checkout the command runs in.
	WorkDir string
	// Dir holds StatusFile and the logs.
	Dir string
	// Environment is the environment the agent's own tools get.
	Environment []string
	// Executable is the harness binary that runs in supervisor mode. Empty
	// uses this process's executable.
	Executable string
}

// Run starts the service's supervisor and waits for it. When ctx ends it asks
// the supervisor to stop the service and waits for it to finish. Run records
// each transition in StatusFile and returns the supervisor's exit error.
func Run(ctx context.Context, spec Spec, logger *slog.Logger) error {
	if len(spec.Command) == 0 {
		return errors.New("workspace service command is empty")
	}
	executable := spec.Executable
	if executable == "" {
		var err error
		if executable, err = os.Executable(); err != nil {
			return failed(spec, fmt.Errorf("locate the harness executable: %w", err))
		}
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return fmt.Errorf("create the workspace service directory: %w", err)
	}
	args := append([]string{SupervisorArg, spec.Dir, "--"}, spec.Command...)
	cmd := exec.Command(executable, args...) //nolint:gosec // The harness re-executes itself; the command is the Session's own declaration.
	cmd.Dir = spec.WorkDir
	cmd.Env = spec.Environment
	// The supervisor's own diagnostics go to the harness's log. It stays in
	// the harness's process group, where the orphan reapers never reap.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	identity, err := start(cmd)
	if err != nil {
		return failed(spec, fmt.Errorf("start the workspace service: %w", err))
	}
	started := time.Now().UTC()
	if err := WriteStatus(spec.Dir, Status{Command: spec.Command, State: "running", PID: identity.PID, StartedAt: &started}); err != nil {
		logger.WarnContext(ctx, "record workspace service status", "error", err)
	}
	logger.InfoContext(ctx, "workspace service started", "pid", identity.PID)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(stopGrace + 5*time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-done
		}
	}
	forget(identity)
	exited := time.Now().UTC()
	code := cmd.ProcessState.ExitCode()
	if err := WriteStatus(spec.Dir, Status{Command: spec.Command, State: "exited", PID: identity.PID, StartedAt: &started, ExitedAt: &exited, ExitCode: &code}); err != nil {
		logger.WarnContext(ctx, "record workspace service status", "error", err)
	}
	logger.InfoContext(ctx, "workspace service exited", "pid", identity.PID, "exit_code", code)
	return waitErr
}

// start launches cmd and registers it as a supervisor under the census lock,
// so no census sees the new process before it is excluded.
func start(cmd *exec.Cmd) (Identity, error) {
	censusMu.Lock()
	defer censusMu.Unlock()
	if err := cmd.Start(); err != nil {
		return Identity{}, err
	}
	// The child cannot be reaped before Wait, so its PID and start time are
	// stable here.
	started, err := startTime(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Identity{}, err
	}
	identity := Identity{PID: cmd.Process.Pid, Started: started}
	roots[identity.PID] = identity
	return identity, nil
}

// forget unregisters a supervisor after Wait reaped it. Its start time keeps a
// process that reuses the PID in between from matching.
func forget(identity Identity) {
	censusMu.Lock()
	defer censusMu.Unlock()
	if roots[identity.PID] == identity {
		delete(roots, identity.PID)
	}
}

func failed(spec Spec, cause error) error {
	now := time.Now().UTC()
	if err := WriteStatus(spec.Dir, Status{Command: spec.Command, State: "failed", ExitedAt: &now, Error: cause.Error()}); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
