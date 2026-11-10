//go:build linux

package sdkrunner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReapOrphans reaps zombie children adopted by this process until ctx ends.
// kagent-claude is PID 1 in the Actor, so tool subprocesses that outlive
// their parents are reparented to it. The bridge leader is skipped so its
// exec.Cmd still observes the exit, and so are zombies in the harness's own
// process group, which may belong to a pending exec.Cmd (Git preparation).
func (s *Session) ReapOrphans(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		leader := 0
		if s.proc != nil {
			leader = s.proc.Pid()
		}
		s.mu.Unlock()
		reapZombies(leader)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reapZombies(skip int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self, group := os.Getpid(), syscall.Getpgrp()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == skip {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// Fields after the parenthesized command: state ppid pgrp ...
		stat := string(data)
		end := strings.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(stat[end+1:])
		if len(fields) < 3 || fields[0] != "Z" {
			continue
		}
		parent, _ := strconv.Atoi(fields[1])
		processGroup, _ := strconv.Atoi(fields[2])
		if parent != self || processGroup == group {
			continue
		}
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	}
}
