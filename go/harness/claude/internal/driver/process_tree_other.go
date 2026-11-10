//go:build !linux

package driver

import (
	"fmt"
	"os/exec"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// processTree outside Linux signals the leader's process group only. It
// exists so the driver builds and its tests run on a developer machine;
// Actors run on Linux, where pidfds, child subreaping and /proc bind cleanup
// to the whole native tree.
type processTree struct{ command *exec.Cmd }

func newProcessTree() (*processTree, error) { return &processTree{}, nil }

func (*processTree) release() {}

func (p *processTree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Claude: %w", err)
	}
	p.command = cmd
	return nil
}

func (*processTree) closeLeader() error { return nil }

func (p *processTree) interrupt() error {
	_ = utils.InterruptProcessGroup(p.command.Process)
	return nil
}

func (p *processTree) kill() error {
	_ = utils.KillProcessGroup(p.command.Process)
	return nil
}

func (*processTree) killAndReap() error { return nil }
