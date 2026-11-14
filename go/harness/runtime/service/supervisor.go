package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Supervise is the supervisor's main: args are the arguments after
// SupervisorArg, the service directory, "--" and the command. It makes itself a
// child subreaper, runs the command with its output in the rotating log, reaps
// every process of the service, and exits once none is left, with the
// command's own exit status. SIGTERM or SIGINT stops the whole service: SIGTERM
// to each of its processes, then SIGKILL after stopGrace.
func Supervise(args []string) int {
	if len(args) < 3 || args[1] != "--" || args[0] == "" || args[2] == "" {
		fmt.Fprintln(os.Stderr, "workspace service: usage: "+SupervisorArg+" <dir> -- <command> [args...]")
		return 2
	}
	dir, command := args[0], args[2:]
	if err := supervise(dir, command); err != nil {
		var exit *exitStatus
		if errors.As(err, &exit) {
			return exit.code
		}
		fmt.Fprintln(os.Stderr, "workspace service:", err)
		return 1
	}
	return 0
}

type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func supervise(dir string, command []string) error {
	// Become the subreaper first: a process orphaned before this would be
	// adopted by the harness instead.
	if err := becomeSubreaper(); err != nil {
		return fmt.Errorf("become a child subreaper: %w", err)
	}
	log, err := openRotatingLog(dir, maxLogBytes)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	path, err := exec.LookPath(command[0])
	if err != nil {
		_, _ = fmt.Fprintf(log, "== workspace service %s: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		return err
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	_, _ = fmt.Fprintf(log, "== workspace service started %s: %q\n", time.Now().UTC().Format(time.RFC3339), command)
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	leader, err := os.StartProcess(path, command, &os.ProcAttr{Dir: wd, Env: os.Environ(), Files: []*os.File{devNull, writer, writer}})
	_ = writer.Close()
	if err != nil {
		_ = reader.Close()
		_, _ = fmt.Fprintf(log, "== workspace service could not start: %v\n", err)
		return err
	}
	copied := make(chan struct{})
	go func() {
		// Ends when every process holding the pipe has exited.
		_, _ = io.Copy(log, reader)
		_ = reader.Close()
		close(copied)
	}()

	// Reap every child, adopted orphans included, until none is left.
	reaped := make(chan int, 1)
	go func() { reaped <- reapAll(leader.Pid) }()

	var kill <-chan time.Time
	for {
		select {
		case code := <-reaped:
			select {
			case <-copied:
			case <-time.After(time.Second):
			}
			_, _ = fmt.Fprintf(log, "== workspace service exited %s: status %d\n", time.Now().UTC().Format(time.RFC3339), code)
			if code != 0 {
				return &exitStatus{code: code}
			}
			return nil
		case <-signals:
			if kill == nil {
				signalTree(leader.Pid, syscall.SIGTERM)
				kill = time.After(stopGrace)
			}
		case <-kill:
			signalTree(leader.Pid, syscall.SIGKILL)
			kill = nil
		}
	}
}

// reapAll waits for every child of this process and returns the leader's exit
// status, or 128 plus the signal that ended it.
func reapAll(leader int) int {
	code := 0
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			// ECHILD: nothing of the service is left.
			return code
		}
		if pid != leader {
			continue
		}
		switch {
		case status.Exited():
			code = status.ExitStatus()
		case status.Signaled():
			code = 128 + int(status.Signal())
		}
	}
}

// rotatingLog keeps the service's output to at most two files of max bytes.
type rotatingLog struct {
	dir  string
	max  int64
	file *os.File
	size int64
}

func openRotatingLog(dir string, max int64) (*rotatingLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	log := &rotatingLog{dir: dir, max: max}
	if err := log.open(); err != nil {
		return nil, err
	}
	return log, nil
}

func (l *rotatingLog) open() error {
	file, err := os.OpenFile(filepath.Join(l.dir, LogFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	l.file, l.size = file, info.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(l.dir, LogFile), filepath.Join(l.dir, RotatedLogFile)); err != nil {
		return err
	}
	return l.open()
}

func (l *rotatingLog) Close() error { return l.file.Close() }
