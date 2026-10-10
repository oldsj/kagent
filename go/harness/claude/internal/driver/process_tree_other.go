//go:build !linux

package driver

import "fmt"

type processTree struct{}

func newProcessTree() (*processTree, error) {
	return nil, fmt.Errorf("Claude process supervision requires Linux child subreaping and /proc")
}

func (*processTree) release()              {}
func (*processTree) interrupt() error      { return nil }
func (*processTree) killAndReap(int) error { return nil }
