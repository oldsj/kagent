package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// markerName lives inside .git so a plain `rm -rf .git` or a fresh checkout both
// reset it, and so it never shows up as an untracked file.
const markerName = "kagent-bootstrap"

// placeholderHeader gives the egress gateway an Authorization header to
// overwrite. The gateway replaces a header only when the client already sent
// one, and the real value never enters the actor.
const placeholderHeader = "Authorization: Basic cGxhY2Vob2xkZXI="

// maxStderr bounds how much of a failed git invocation is kept.
const maxStderr = 8 << 10

var (
	errRefNotFound = errors.New("ref not found")
	errNoGit       = errors.New("git is not installed")
	fullSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// checkout is one fully validated clone request.
type checkout struct {
	Dir    string
	Repo   string
	Host   string
	Ref    string
	Branch string
	Depth  int
	// Credential repo-locally configures the placeholder header for Host.
	Credential bool
}

// gitError is a failed git invocation. Its stderr is shown only after
// failureMessage vets and shortens it.
type gitError struct {
	Step   string
	Stderr string
	Err    error
}

func (e *gitError) Error() string { return fmt.Sprintf("git %s: %v: %s", e.Step, e.Err, e.Stderr) }
func (e *gitError) Unwrap() error { return e.Err }

type gitRunner struct {
	path        string
	pathErr     error
	environment []string
}

func newGitRunner(environment []string) (*gitRunner, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		err = errNoGit
	}
	// Hermetic: no user or system config can redirect the clone, and git must
	// never prompt, since a prompt would hang the first turn.
	environment = append(append([]string(nil), environment...),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C")
	return &gitRunner{path: path, pathErr: err, environment: environment}, nil
}

func bootstrapped(dir string) bool {
	_, err := os.Stat(markerPath(dir))
	return err == nil
}

func markerPath(dir string) string { return filepath.Join(dir, ".git", markerName) }

// checkout builds the repository from scratch and writes the marker last, so an
// interrupted attempt is always redone from a clean .git. That is safe because
// bootstrap runs before the first turn: a .git without a marker is ours.
func (g *gitRunner) checkout(ctx context.Context, c checkout) error {
	if g.pathErr != nil {
		return g.pathErr
	}
	if err := os.RemoveAll(filepath.Join(c.Dir, ".git")); err != nil {
		return fmt.Errorf("reset partial checkout: %w", err)
	}
	for _, name := range []string{c.Ref, c.Branch} {
		if err := checkRefName(ctx, g, name); err != nil {
			return err
		}
	}
	if _, err := g.run(ctx, "init", "init", "-q", c.Dir); err != nil {
		return err
	}
	if c.Credential {
		if _, err := g.git(ctx, c.Dir, "config", "config", "--local", "http.https://"+c.Host+"/.extraHeader", placeholderHeader); err != nil {
			return err
		}
	}
	if _, err := g.git(ctx, c.Dir, "remote add", "remote", "add", "origin", c.Repo); err != nil {
		return err
	}
	if err := g.fetchRef(ctx, c); err != nil {
		return err
	}
	if c.Branch != "" {
		if _, err := g.git(ctx, c.Dir, "checkout", "checkout", "-q", "-B", c.Branch); err != nil {
			return err
		}
	}
	return writeMarker(c)
}

// fetchRef fetches and checks out c.Ref: a full commit SHA, a branch, or a tag,
// or the remote's default branch when empty.
func (g *gitRunner) fetchRef(ctx context.Context, c checkout) error {
	depth := "--depth=" + strconv.Itoa(c.Depth)
	switch {
	case fullSHA.MatchString(c.Ref):
		if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", c.Ref); err != nil {
			return notFoundOr(err)
		}
		_, err := g.git(ctx, c.Dir, "checkout", "checkout", "-q", "--detach", "FETCH_HEAD")
		return err
	case c.Ref == "":
		return g.fetchBranch(ctx, c, g.defaultBranch(ctx, c))
	}
	err := g.fetchBranch(ctx, c, c.Ref)
	if !errors.Is(err, errRefNotFound) {
		return err
	}
	tag := "refs/tags/" + c.Ref
	if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "origin", "+"+tag+":"+tag); err != nil {
		return notFoundOr(err)
	}
	_, err = g.git(ctx, c.Dir, "checkout", "checkout", "-q", "--detach", tag)
	return err
}

func (g *gitRunner) fetchBranch(ctx context.Context, c checkout, branch string) error {
	depth := "--depth=" + strconv.Itoa(c.Depth)
	if branch == "" {
		// The remote did not name a default branch; fall back to its HEAD.
		if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", "HEAD"); err != nil {
			return notFoundOr(err)
		}
		_, err := g.git(ctx, c.Dir, "checkout", "checkout", "-q", "--detach", "FETCH_HEAD")
		return err
	}
	remote := "refs/remotes/origin/" + branch
	if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", "+refs/heads/"+branch+":"+remote); err != nil {
		return notFoundOr(err)
	}
	if _, err := g.git(ctx, c.Dir, "checkout", "checkout", "-q", "-B", branch, remote); err != nil {
		return err
	}
	_, err := g.git(ctx, c.Dir, "branch", "branch", "-q", "--set-upstream-to=origin/"+branch)
	return err
}

// defaultBranch asks the remote which branch HEAD points at, or returns "".
func (g *gitRunner) defaultBranch(ctx context.Context, c checkout) string {
	out, err := g.git(ctx, c.Dir, "ls-remote", "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if name, _, ok := strings.Cut(rest, "\t"); ok {
				return name
			}
		}
	}
	return ""
}

// checkRefName rejects names git would not accept as a branch, and anything
// that could be read as an option. The proto already restricts the characters.
func checkRefName(ctx context.Context, g *gitRunner, name string) error {
	if name == "" {
		return nil
	}
	if strings.HasPrefix(name, "-") {
		return &gitError{Step: "check-ref-format", Err: errors.New("invalid ref name")}
	}
	if _, err := g.run(ctx, "check-ref-format", "check-ref-format", "--allow-onelevel", name); err != nil {
		return &gitError{Step: "check-ref-format", Err: errors.New("invalid ref name")}
	}
	return nil
}

func writeMarker(c checkout) error {
	body := fmt.Sprintf("repo=%s\nref=%s\nbranch=%s\ncompleted=%s\n", c.Repo, c.Ref, c.Branch, time.Now().UTC().Format(time.RFC3339))
	tmp, err := os.CreateTemp(filepath.Join(c.Dir, ".git"), markerName+".tmp-*")
	if err != nil {
		return fmt.Errorf("write bootstrap marker: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write bootstrap marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write bootstrap marker: %w", err)
	}
	if err := os.Rename(tmp.Name(), markerPath(c.Dir)); err != nil {
		return fmt.Errorf("write bootstrap marker: %w", err)
	}
	return nil
}

func (g *gitRunner) git(ctx context.Context, dir, step string, args ...string) (string, error) {
	return g.run(ctx, step, append([]string{"-C", dir}, args...)...)
}

// run executes git and returns stdout. Callers
// that need a working directory use git, which passes -C.
func (g *gitRunner) run(ctx context.Context, step string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.path, args...)
	cmd.Env = g.environment
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	stderr := &boundedBuffer{limit: maxStderr}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &gitError{Step: step, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.String(), nil
}

// notFoundOr maps git's missing-ref error to errRefNotFound.
func notFoundOr(err error) error {
	var gitErr *gitError
	if errors.As(err, &gitErr) && strings.Contains(gitErr.Stderr, "couldn't find remote ref") {
		return errRefNotFound
	}
	return err
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
