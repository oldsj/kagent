//go:build !linux

package service

import "syscall"

// Outside Linux there is no child subreaper. That is for development only:
// the service's orphans are adopted by init rather than the supervisor.
func becomeSubreaper() error { return nil }

func startTime(int) (uint64, error) { return 0, nil }

// signalTree signals only the leader outside Linux.
func signalTree(leader int, signal syscall.Signal) { _ = syscall.Kill(leader, signal) }
