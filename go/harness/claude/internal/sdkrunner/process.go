package sdkrunner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Process is one running bridge. Stdin carries operations and Stdout events.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Wait returns once the bridge has exited and been reaped.
	Wait() error
	// Stop closes stdin, waits up to grace, then kills the bridge's process
	// group and waits for the leader. It is safe to call more than once.
	Stop(grace time.Duration) error
	// Pid is the bridge leader, which the orphan reaper leaves to Wait.
	Pid() int
}

// Launcher starts a bridge process.
type Launcher interface {
	Launch(ctx context.Context) (Process, error)
}

// NodeLauncher starts `node [--icu-data-dir=ICUDataDir] <Script>` in its own
// process group with Env.
type NodeLauncher struct {
	Node   string
	Script string
	// ICUDataDir is passed as a flag, not NODE_ICU_DATA, so it never reaches
	// Claude or the tools Claude runs, which may start their own Node.
	ICUDataDir string
	Dir        string
	Env        []string
	Stderr     io.Writer
}

func (l NodeLauncher) Launch(_ context.Context) (Process, error) {
	args := []string{l.Script}
	if l.ICUDataDir != "" {
		args = append([]string{"--icu-data-dir=" + l.ICUDataDir}, args...)
	}
	cmd := exec.Command(l.Node, args...)
	cmd.Dir = l.Dir
	cmd.Env = l.Env
	cmd.Stderr = l.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// Its own group lets Stop signal the bridge, the Claude CLI and the CLI's
	// tool subprocesses together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// An explicit pipe rather than StdoutPipe: Wait runs concurrently with the
	// reader, and StdoutPipe's reader may not be used once Wait has started.
	stdout, writer, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdout = writer
	err = cmd.Start()
	_ = writer.Close()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	p := &nodeProcess{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

type nodeProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File
	done   chan struct{}
	err    error
}

func (p *nodeProcess) Pid() int              { return p.cmd.Process.Pid }
func (p *nodeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *nodeProcess) Stdout() io.Reader     { return p.stdout }

func (p *nodeProcess) Wait() error {
	<-p.done
	return p.err
}

func (p *nodeProcess) Stop(grace time.Duration) error {
	_ = p.stdin.Close()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		timer.Reset(grace)
		select {
		case <-p.done:
		case <-timer.C:
		}
	}
	// Remaining group members (tool subprocesses) go with the bridge.
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	timer.Reset(grace)
	select {
	case <-p.done:
		// Unblock a reader whose pipe a detached descendant still holds.
		_ = p.stdout.Close()
		return nil
	case <-timer.C:
		return errors.New("sdk runner did not exit after SIGKILL")
	}
}
