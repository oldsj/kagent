//go:build linux

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

type process struct {
	pid, parent int
	started     uint64
}

func startTime(pid int) (uint64, error) {
	p, err := readProcess(pid)
	return p.started, err
}

// signalTree signals every descendant of this process, the leader among them.
// Each target is opened as a pidfd and its identity rechecked, so a reused PID
// is never signaled.
func signalTree(_ int, signal syscall.Signal) {
	processes, err := readProcesses()
	if err != nil {
		fmt.Fprintln(os.Stderr, "workspace service: list processes:", err)
		return
	}
	for _, target := range descendants(processes, os.Getpid()) {
		fd, err := unix.PidfdOpen(target.pid, 0)
		if err != nil {
			continue
		}
		if current, err := readProcess(target.pid); err == nil && current == target {
			_ = unix.PidfdSendSignal(fd, signal, nil, 0)
		}
		_ = unix.Close(fd)
	}
}

func descendants(processes map[int]process, root int) []process {
	var result []process
	parents := []int{root}
	for len(parents) > 0 {
		parent := parents[0]
		parents = parents[1:]
		for _, p := range processes {
			if p.parent == parent && p.pid != root {
				result = append(result, p)
				parents = append(parents, p.pid)
			}
		}
	}
	return result
}

func readProcesses() (map[int]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	processes := map[int]process{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		p, err := readProcess(pid)
		if err != nil {
			continue // Exited during the scan.
		}
		processes[pid] = p
	}
	return processes, nil
}

func readProcess(pid int) (process, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return process{}, err
	}
	return parseStat(pid, string(data))
}

func parseStat(pid int, stat string) (process, error) {
	// The parenthesized command can contain both spaces and parentheses.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return process{}, fmt.Errorf("invalid process %d stat", pid)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return process{}, fmt.Errorf("incomplete process %d stat", pid)
	}
	parent, parentErr := strconv.Atoi(fields[1])
	started, startErr := strconv.ParseUint(fields[19], 10, 64)
	if err := errors.Join(parentErr, startErr); err != nil {
		return process{}, fmt.Errorf("invalid process %d stat: %w", pid, err)
	}
	return process{pid: pid, parent: parent, started: started}, nil
}
