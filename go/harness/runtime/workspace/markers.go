package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

const (
	startedName = "workspace-bootstrap.started"
	doneName    = "workspace-bootstrap.done"
)

// errForeignGit means the workspace holds a .git that this bootstrap did not
// start. It is never removed: it may carry the agent's unpushed commits.
var errForeignGit = errors.New("workspace has a Git repository that the bootstrap did not create")

// markers records how far this bootstrap got. They live in a durable directory
// outside the workspace, so nothing an agent does inside the workspace (git
// init, a fresh clone, rm -rf .git) can forge or erase them, and they never
// appear as untracked files.
//
// started is written before the first change to the workspace. done replaces it
// once the checkout is complete. A started marker with no done marker is
// therefore our own interrupted attempt and the only state that may be reset.
type markers struct{ dir string }

func (m markers) started() bool { return exists(filepath.Join(m.dir, startedName)) }
func (m markers) done() bool    { return exists(filepath.Join(m.dir, doneName)) }

// markStarted records the attempt before anything in the workspace changes.
func (m markers) markStarted(c checkout) error {
	return m.write(startedName, c, "")
}

// markDone writes done first and only then drops started. Once done is durable
// the bootstrap has succeeded, so a failed removal is not an error: clearStale
// removes the leftover on a later turn. Both files present still reads as done.
func (m markers) markDone(ctx context.Context, c checkout) error {
	if err := m.write(doneName, c, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	m.clearStale(ctx)
	return nil
}

// clearStale removes a started marker that outlived its done marker, on a best
// effort basis: a failure is logged, not returned. It must run while done
// exists: if started were still there when done is later removed, plan would
// read the agent's own .git as our interrupted attempt and reset it. With
// neither marker left, that .git is foreign and stays untouched.
func (m markers) clearStale(ctx context.Context) {
	if !m.done() {
		return
	}
	path := filepath.Join(m.dir, startedName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logging.FromContext(ctx).WarnContext(ctx, "failed to remove stale bootstrap start marker", "path", path, "error", err)
	}
}

// write stores the marker atomically. The body names the checkout and holds no
// credentials.
func (m markers) write(name string, c checkout, completed string) error {
	body := fmt.Sprintf("repo=%s\nref=%s\nbranch=%s\ncompleted=%s\n", c.Repo, c.Ref, c.Branch, completed)
	if err := utils.ReplacePrivateFile(filepath.Join(m.dir, name), []byte(body)); err != nil {
		return fmt.Errorf("write bootstrap marker: %w", err)
	}
	return nil
}

// action is what the bootstrap does about the workspace's current state.
type action int

const (
	// actionNone: bootstrap already finished; leave the workspace to the agent.
	actionNone action = iota
	// actionClone: nothing is there yet.
	actionClone
	// actionReset: our own attempt was interrupted; drop its .git and redo it.
	actionReset
	// actionForeign: a .git we did not create; fail without touching it.
	actionForeign
)

// plan decides the action from the markers and whether the workspace has a .git.
// done wins over everything, so an agent that removes .git or replaces it never
// gets it deleted or re-cloned.
func plan(done, started, hasGit bool) action {
	switch {
	case done:
		return actionNone
	case started:
		return actionReset
	case hasGit:
		return actionForeign
	default:
		return actionClone
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
