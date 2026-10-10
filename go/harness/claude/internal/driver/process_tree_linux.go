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
	preexisting map[int]uint64
	released    bool
}

func newProcessTree() (*processTree, error) {
	if !nativeTreeActive.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("another Claude process tree is still owned by this harness")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		nativeTreeActive.Store(false)
		return nil, fmt.Errorf("enable Claude child subreaper: %w", err)
	}
	processes, err := readProcesses()
	if err != nil {
		nativeTreeActive.Store(false)
		return nil, err
	}
	tree := &processTree{preexisting: make(map[int]uint64)}
	// Leave existing child trees alone, including processes owned by an
	// embedder. The dedicated Actor starts no other child tree during a turn.
	for _, process := range descendants(processes, os.Getpid(), nil) {
		tree.preexisting[process.pid] = process.started
	}
	return tree, nil
}

func (p *processTree) release() {
	if !p.released {
		p.released = true
		nativeTreeActive.Store(false)
	}
}

func (p *processTree) interrupt() error { return p.signal(unix.SIGINT) }

func (p *processTree) signal(signal unix.Signal) error {
	processes, err := readProcesses()
	if err != nil {
		return err
	}
	var failures []error
	for _, process := range descendants(processes, os.Getpid(), p.preexisting) {
		if err := signalProcess(process, signal); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// killAndReap rescans after every kill: descendants that orphan during cleanup
// become our direct children, even after setsid, double-fork, or an env reset.
// Wait only for adopted children; exec.Cmd owns reaping the original leader.
func (p *processTree) killAndReap(leader int) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		processes, err := readProcesses()
		if err != nil {
			return err
		}
		remaining := descendants(processes, os.Getpid(), p.preexisting)
		if len(remaining) == 0 {
			return nil
		}
		var failures []error
		for _, process := range remaining {
			if err := signalProcess(process, unix.SIGKILL); err != nil {
				failures = append(failures, err)
			}
			if process.parent == os.Getpid() && process.pid != leader {
				var status unix.WaitStatus
				if _, err := unix.Wait4(process.pid, &status, unix.WNOHANG, nil); err != nil && !errors.Is(err, unix.ECHILD) && !errors.Is(err, unix.EINTR) {
					failures = append(failures, fmt.Errorf("reap Claude descendant %d: %w", process.pid, err))
				}
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
	current, err := readProcess(process.pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	// A PID reused after our snapshot belongs to another process.
	if current.started != process.started {
		return nil
	}
	if err := unix.Kill(process.pid, signal); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("signal Claude descendant %d: %w", process.pid, err)
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

func descendants(processes map[int]processIdentity, root int, exclude map[int]uint64) []processIdentity {
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
			if started, present := exclude[process.pid]; present && started == process.started {
				continue
			}
			result = append(result, process)
			parents = append(parents, process.pid)
		}
	}
	return result
}
