//go:build !linux

package driver

import "fmt"

type processTree struct{}

func newProcessTree() (*processTree, error) {
	return nil, fmt.Errorf("Claude process supervision requires Linux pidfds, child subreaping and /proc")
}

func (*processTree) release()           {}
func (*processTree) interrupt() error   { return nil }
func (*processTree) kill() error        { return nil }
func (*processTree) killAndReap() error { return nil }
