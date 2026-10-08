package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
)

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
	Dir string
	// StateDir is the durable directory that holds the bootstrap markers. It must
	// be outside Dir.
	StateDir string
	Repo     string
	Host     string
	Ref      string
	Branch   string
	Depth    int
	// Credential repo-locally configures the placeholder header for Host.
	Credential bool
	ReadURL    string
	PushURL    string
	Proxy      bool
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

func bootstrapped(stateDir string) bool { return markers{stateDir}.done() }

// checkout builds the repository once. It writes a started marker before the
// first change and a done marker last, both outside the workspace (see markers).
//
//   - done: nothing to do, whatever the workspace holds now.
//   - started without done: our own interrupted attempt. Its .git is removed
//     and the checkout redone with -f, because the first attempt may already
//     have written files that would otherwise block it.
//   - neither marker but a .git: not ours, so fail and leave it untouched.
func (g *gitRunner) checkout(ctx context.Context, c checkout) error {
	if g.pathErr != nil {
		return g.pathErr
	}
	var proxyOrigins []string
	if c.Proxy {
		if c.Credential || c.ReadURL == "" || c.PushURL == "" {
			return errors.New("invalid Git proxy transport")
		}
		for i, rawURL := range []string{c.ReadURL, c.PushURL} {
			u, err := url.Parse(rawURL)
			if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("invalid Git proxy transport")
			}
			origin := u.Scheme + "://" + u.Host
			if origin != apiworkspace.ReadProxyOrigin && (i == 0 || origin != apiworkspace.PushProxyOrigin) {
				return errors.New("invalid Git proxy transport")
			}
			proxyOrigins = append(proxyOrigins, origin)
		}
	}
	for _, name := range []string{c.Ref, c.Branch} {
		if err := checkRefName(ctx, g, name); err != nil {
			return err
		}
	}
	m := markers{c.StateDir}
	hasGit := exists(filepath.Join(c.Dir, ".git"))
	force := false
	switch plan(m.done(), m.started(), hasGit) {
	case actionNone:
		return nil
	case actionForeign:
		return errForeignGit
	case actionReset:
		force = true
		if err := os.RemoveAll(filepath.Join(c.Dir, ".git")); err != nil {
			return fmt.Errorf("reset partial checkout: %w", err)
		}
	}
	if err := m.markStarted(c); err != nil {
		return err
	}
	if _, err := g.run(ctx, "init", "init", "-q", c.Dir); err != nil {
		return err
	}
	if c.Credential {
		if _, err := g.git(ctx, c.Dir, "config", "config", "--local", "http.https://"+c.Host+"/.extraHeader", placeholderHeader); err != nil {
			return err
		}
	}
	if c.Proxy {
		for _, origin := range proxyOrigins {
			// A root origin scopes the inert placeholder to this listener.
			if _, err := g.git(ctx, c.Dir, "proxy header", "config", "--local", "http."+origin+"/.extraHeader", placeholderHeader); err != nil {
				return err
			}
		}
		if _, err := g.git(ctx, c.Dir, "proxy redirects", "config", "--local", "http.followRedirects", "false"); err != nil {
			return err
		}
	}
	readURL := c.ReadURL
	if readURL == "" {
		readURL = c.Repo // Explicit standalone checkout, never a proxy fallback.
	}
	if _, err := g.git(ctx, c.Dir, "remote add", "remote", "add", "origin", readURL); err != nil {
		return err
	}
	if c.PushURL != "" {
		if _, err := g.git(ctx, c.Dir, "push URL", "config", "--local", "remote.origin.pushurl", c.PushURL); err != nil {
			return err
		}
	}
	if err := g.fetchRef(ctx, c, force); err != nil {
		return err
	}
	if c.Branch != "" {
		if _, err := g.git(ctx, c.Dir, "checkout", checkoutArgs(force, "-B", c.Branch)...); err != nil {
			return err
		}
	}
	return m.markDone(ctx, c)
}

// fetchRef fetches and checks out c.Ref: a full commit SHA, a branch, or a tag,
// or the remote's default branch when empty.
func (g *gitRunner) fetchRef(ctx context.Context, c checkout, force bool) error {
	depth := "--depth=" + strconv.Itoa(c.Depth)
	switch {
	case fullSHA.MatchString(c.Ref):
		if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", c.Ref); err != nil {
			return notFoundOr(err)
		}
		_, err := g.git(ctx, c.Dir, "checkout", checkoutArgs(force, "--detach", "FETCH_HEAD")...)
		return err
	case c.Ref == "":
		return g.fetchBranch(ctx, c, g.defaultBranch(ctx, c), force)
	}
	err := g.fetchBranch(ctx, c, c.Ref, force)
	if !errors.Is(err, errRefNotFound) {
		return err
	}
	tag := "refs/tags/" + c.Ref
	if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "origin", "+"+tag+":"+tag); err != nil {
		return notFoundOr(err)
	}
	_, err = g.git(ctx, c.Dir, "checkout", checkoutArgs(force, "--detach", tag)...)
	return err
}

func (g *gitRunner) fetchBranch(ctx context.Context, c checkout, branch string, force bool) error {
	depth := "--depth=" + strconv.Itoa(c.Depth)
	if branch == "" {
		// The remote did not name a default branch; fall back to its HEAD.
		if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", "HEAD"); err != nil {
			return notFoundOr(err)
		}
		_, err := g.git(ctx, c.Dir, "checkout", checkoutArgs(force, "--detach", "FETCH_HEAD")...)
		return err
	}
	remote := "refs/remotes/origin/" + branch
	if _, err := g.git(ctx, c.Dir, "fetch", "fetch", "-q", depth, "--no-tags", "origin", "+refs/heads/"+branch+":"+remote); err != nil {
		return notFoundOr(err)
	}
	if _, err := g.git(ctx, c.Dir, "checkout", checkoutArgs(force, "-B", branch, remote)...); err != nil {
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

// checkoutArgs builds a quiet `git checkout`. force (-f) discards untracked
// files in the way; it is set only when redoing our own interrupted attempt,
// whose earlier checkout may have written files into the workspace.
func checkoutArgs(force bool, args ...string) []string {
	base := []string{"checkout", "-q"}
	if force {
		base = append(base, "-f")
	}
	return append(base, args...)
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
