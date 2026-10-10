//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// An Actor has one native turn, including while parked for approval. Keep that
// ownership process-wide: adopted orphans no longer identify their old leader.
// Reject concurrent Claude trees rather than guessing which turn owns them.
var nativeTreeActive atomic.Bool

type processIdentity struct {
	pid, parent int
	started     uint64
}

type processTree struct {
	released bool
}

func newProcessTree() (_ *processTree, err error) {
	if !nativeTreeActive.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("another Claude process tree is still owned by this harness")
	}
	defer func() {
		if err != nil {
			nativeTreeActive.Store(false)
		}
	}()
	// Reject unsupported kernels or syscall filters before launching Claude.
	// Signal 0 checks support without delivering a signal.
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, fmt.Errorf("open Claude supervision pidfd: %w", err)
	}
	signalErr := unix.PidfdSendSignal(fd, 0, nil, 0)
	// Reaping adopted children also needs waitid's stable-handle mode.
	waitErr := unix.Waitid(unix.P_PIDFD, fd, nil, unix.WEXITED|unix.WNOHANG, nil)
	if errors.Is(waitErr, unix.ECHILD) {
		waitErr = nil // The harness cannot wait for itself.
	}
	if err := errors.Join(signalErr, waitErr, unix.Close(fd)); err != nil {
		return nil, fmt.Errorf("check Claude pidfd supervision support: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("enable Claude child subreaper: %w", err)
	}
	processes, err := readProcesses()
	if err != nil {
		return nil, err
	}
	// A census cannot exclude future orphans of an existing child tree: once
	// adopted, their ancestry is lost. The dedicated Actor must have no other
	// descendants before launch and start no unrelated trees during this turn.
	if len(descendants(processes, os.Getpid())) != 0 {
		return nil, fmt.Errorf("Claude process supervision requires no pre-existing descendant trees")
	}
	return &processTree{}, nil
}

func (p *processTree) release() {
	if !p.released {
		p.released = true
		nativeTreeActive.Store(false)
	}
}

func (p *processTree) interrupt() error { return p.signal(unix.SIGINT) }
func (p *processTree) kill() error      { return p.signal(unix.SIGKILL) }

func (p *processTree) signal(signal unix.Signal) error {
	if p.released {
		return fmt.Errorf("Claude process ownership has been released")
	}
	processes, err := readProcesses()
	if err != nil {
		return err
	}
	var failures []error
	for _, process := range descendants(processes, os.Getpid()) {
		if err := signalProcess(process, signal); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// killAndReap rescans after every kill: descendants that orphan during cleanup
// become our direct children, even after setsid, double-fork, or an env reset.
// exec.Cmd must have reaped the original leader before this is called, so all
// remaining direct children are adopted and can be reaped by their pidfds.
func (p *processTree) killAndReap() error {
	if p.released {
		return fmt.Errorf("Claude process ownership has been released")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		processes, err := readProcesses()
		if err != nil {
			return err
		}
		remaining := descendants(processes, os.Getpid())
		if len(remaining) == 0 {
			return nil
		}
		var failures []error
		for _, process := range remaining {
			handle, err := openProcess(process)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if handle == nil {
				continue
			}
			if err := handle.signal(unix.SIGKILL); err != nil {
				failures = append(failures, err)
			}
			if process.parent == os.Getpid() {
				if err := unix.Waitid(unix.P_PIDFD, handle.fd, nil, unix.WEXITED|unix.WNOHANG, nil); err != nil && !errors.Is(err, unix.ECHILD) && !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.ESRCH) {
					failures = append(failures, fmt.Errorf("reap Claude descendant %d: %w", process.pid, err))
				}
			}
			if err := unix.Close(handle.fd); err != nil {
				failures = append(failures, fmt.Errorf("close Claude descendant %d pidfd: %w", process.pid, err))
			}
		}
		if err := errors.Join(failures...); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Claude descendant cleanup did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func signalProcess(process processIdentity, signal unix.Signal) error {
	handle, err := openProcess(process)
	if err != nil || handle == nil {
		return err
	}
	return errors.Join(handle.signal(signal), unix.Close(handle.fd))
}

type processHandle struct {
	pid int
	fd  int
}

// Open before rechecking the census identity. Even if Wait reaps the process
// after the check, signaling this handle cannot reach a replacement PID.
func openProcess(process processIdentity) (*processHandle, error) {
	fd, err := unix.PidfdOpen(process.pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Claude descendant %d pidfd: %w", process.pid, err)
	}
	current, err := readProcess(process.pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil, unix.Close(fd)
	}
	if err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	if current != process {
		return nil, unix.Close(fd)
	}
	return &processHandle{pid: process.pid, fd: fd}, nil
}

func (p *processHandle) signal(signal unix.Signal) error {
	if err := unix.PidfdSendSignal(p.fd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("signal Claude descendant %d: %w", p.pid, err)
	}
	return nil
}

func readProcesses() (map[int]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read Claude process ownership: %w", err)
	}
	processes := make(map[int]processIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		process, err := readProcess(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		processes[pid] = process
	}
	return processes, nil
}

func readProcess(pid int) (processIdentity, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process %d identity: %w", pid, err)
	}
	return parseProcessIdentity(pid, string(data))
}

func parseProcessIdentity(pid int, stat string) (processIdentity, error) {
	// The parenthesized command can contain both spaces and parentheses.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return processIdentity{}, fmt.Errorf("invalid process %d identity", pid)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return processIdentity{}, fmt.Errorf("incomplete process %d identity", pid)
	}
	parent, parentErr := strconv.Atoi(fields[1])
	started, startErr := strconv.ParseUint(fields[19], 10, 64)
	if parentErr != nil || startErr != nil {
		return processIdentity{}, fmt.Errorf("invalid process %d ancestry: %w", pid, errors.Join(parentErr, startErr))
	}
	return processIdentity{pid: pid, parent: parent, started: started}, nil
}

func descendants(processes map[int]processIdentity, root int) []processIdentity {
	var result []processIdentity
	parents := []int{root}
	visited := map[int]bool{root: true}
	for len(parents) > 0 {
		parent := parents[0]
		parents = parents[1:]
		for _, process := range processes {
			if process.parent != parent || visited[process.pid] {
				continue
			}
			visited[process.pid] = true
			result = append(result, process)
			parents = append(parents, process.pid)
		}
	}
	return result
}
