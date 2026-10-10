package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

var gitIdentity = []string{
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(gitIdentity, "GIT_CONFIG_GLOBAL=/dev/null")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo builds a bare repository with main, a dev branch, and a v1 tag, and
// returns its file:// URL (--depth is ignored for plain local paths) and main's SHA.
func originRepo(t *testing.T) (url, sha string) {
	t.Helper()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "origin.git")
	run(t, work, "init", "-q", "-b", "main")
	for i, file := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(work, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, work, "add", ".")
		run(t, work, "commit", "-q", "-m", "c"+string(rune('0'+i)))
	}
	sha = run(t, work, "rev-parse", "HEAD")
	run(t, work, "tag", "v1")
	run(t, work, "checkout", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(work, "dev-only"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "dev")
	run(t, work, "checkout", "-q", "main")
	run(t, "", "clone", "-q", "--bare", work, bare)
	return "file://" + bare, sha
}

func newRunner(t *testing.T) *gitRunner {
	t.Helper()
	g, err := newGitRunner(append(os.Environ(), gitIdentity...))
	if err != nil || g.pathErr != nil {
		t.Skip("git is not available")
	}
	return g
}

func TestCheckout(t *testing.T) {
	url, sha := originRepo(t)
	tests := []struct {
		name       string
		ref        string
		branch     string
		wantBranch string
		wantFile   string
		wantAbsent string
	}{
		{name: "default branch", wantBranch: "main", wantFile: "b"},
		{name: "named branch", ref: "dev", wantBranch: "dev", wantFile: "dev-only"},
		{name: "tag is detached", ref: "v1", wantBranch: "", wantFile: "b"},
		{name: "commit is detached", ref: sha, wantBranch: "", wantFile: "b"},
		{name: "new branch from ref", ref: "main", branch: "spike-b-test", wantBranch: "spike-b-test", wantFile: "b", wantAbsent: "dev-only"},
		{name: "branch equal to ref", ref: "dev", branch: "dev", wantBranch: "dev", wantFile: "dev-only"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
			err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: test.ref, Branch: test.branch, Depth: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !bootstrapped(state) {
				t.Fatal("marker missing after a successful checkout")
			}
			if _, err := os.Stat(filepath.Join(dir, test.wantFile)); err != nil {
				t.Fatalf("expected file %s: %v", test.wantFile, err)
			}
			if test.wantAbsent != "" {
				if _, err := os.Stat(filepath.Join(dir, test.wantAbsent)); err == nil {
					t.Fatalf("file %s should not be checked out", test.wantAbsent)
				}
			}
			branch, _ := g.git(context.Background(), dir, "branch", "branch", "--show-current")
			if strings.TrimSpace(branch) != test.wantBranch {
				t.Fatalf("branch = %q, want %q", strings.TrimSpace(branch), test.wantBranch)
			}
		})
	}
}

func TestCheckoutIsShallow(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "main", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if count := run(t, dir, "rev-list", "--count", "HEAD"); count != "1" {
		t.Fatalf("commit count = %s, want 1", count)
	}
}

func TestCheckoutPlaceholderHeaderIsRepoLocal(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "main", Depth: 1, Host: "github.com", Credential: true}); err != nil {
		t.Fatal(err)
	}
	got := run(t, dir, "config", "--local", "--get", "http.https://github.com/.extraHeader")
	if got != placeholderHeader {
		t.Fatalf("extraHeader = %q", got)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(state, doneName))), "Basic") {
		t.Fatal("marker must not contain credentials")
	}
}

func TestCheckoutWithoutCredentialSendsNoHeader(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "main", Depth: 1, Host: "github.com"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "--local", "--get-regexp", "extraheader")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("anonymous checkout configured a header: %s", out)
	}
}

// A detached auto-maintenance child would outlive git and leave the PID 1
// harness with a zombie that blocks Claude process supervision.
func TestCheckoutStartsNoAutoMaintenance(t *testing.T) {
	url, _ := originRepo(t)
	trace := filepath.Join(t.TempDir(), "trace2")
	g, err := newGitRunner(append(os.Environ(), append(gitIdentity, "GIT_TRACE2="+trace)...))
	if err != nil || g.pathErr != nil {
		t.Skip("git is not available")
	}
	dir, state := t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "dev", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	data := string(mustRead(t, trace))
	if !strings.Contains(data, "child_start") {
		t.Fatal("trace recorded no child processes; it cannot show maintenance is off")
	}
	for _, line := range strings.Split(data, "\n") {
		if strings.Contains(line, "child_start") && (strings.Contains(line, " maintenance ") || strings.Contains(line, " gc ")) {
			t.Fatalf("checkout started automatic maintenance: %s", line)
		}
	}
}

// agentCommit makes a commit the way an agent would: a new file and a local commit.
func agentCommit(t *testing.T, dir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agent-work"), []byte("unpushed"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "agent-work")
	run(t, dir, "commit", "-q", "-m", "agent work")
	return run(t, dir, "rev-parse", "HEAD")
}

func TestCheckoutBootstrapStates(t *testing.T) {
	url, sha := originRepo(t)
	complete := func(t *testing.T, g *gitRunner, c checkout) {
		t.Helper()
		if err := g.checkout(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// setup brings the workspace to the state under test and returns the HEAD
		// that the agent left there, or "" when no agent work exists.
		setup   func(t *testing.T, g *gitRunner, c *checkout) (agentHead string)
		wantErr error
		// wantHead is origin's main ("origin") or the agent's own HEAD ("agent").
		wantHead string
		// wantDone is whether the done marker must exist afterwards.
		wantDone bool
		verify   func(t *testing.T, c checkout)
	}{
		{
			name:     "fresh clone",
			setup:    func(*testing.T, *gitRunner, *checkout) string { return "" },
			wantHead: "origin", wantDone: true,
		},
		{
			name: "resume after done does nothing and needs no network",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				complete(t, g, *c)
				head := agentCommit(t, c.Dir)
				c.Repo = "file:///does/not/exist"
				return head
			},
			wantHead: "agent", wantDone: true,
		},
		{
			name: "agent re-inits the workspace after done",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				complete(t, g, *c)
				if err := os.RemoveAll(filepath.Join(c.Dir, ".git")); err != nil {
					t.Fatal(err)
				}
				run(t, c.Dir, "init", "-q")
				return agentCommit(t, c.Dir)
			},
			wantHead: "agent", wantDone: true,
		},
		{
			name: "crash after started with a stale lock keeps unrelated files",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				if err := (markers{c.StateDir}).markStarted(*c); err != nil {
					t.Fatal(err)
				}
				run(t, c.Dir, "init", "-q")
				if err := os.WriteFile(filepath.Join(c.Dir, ".git", "index.lock"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(c.Dir, "scratch"), []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantHead: "origin", wantDone: true,
			verify: func(t *testing.T, c checkout) {
				if got := mustRead(t, filepath.Join(c.Dir, "scratch")); string(got) != "keep" {
					t.Fatal("recovery must leave unrelated files alone")
				}
			},
		},
		{
			name: "crash after started with files already checked out",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				complete(t, g, *c)
				// Rewind to the moment of the crash: files are on disk, .git is
				// half-built (no objects), and only the started marker exists.
				state := markers{c.StateDir}
				if err := os.Remove(filepath.Join(c.StateDir, doneName)); err != nil {
					t.Fatal(err)
				}
				if err := state.markStarted(*c); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(c.Dir, ".git")); err != nil {
					t.Fatal(err)
				}
				run(t, c.Dir, "init", "-q")
				if err := os.WriteFile(filepath.Join(c.Dir, "a"), []byte("edited"), 0o644); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantHead: "origin", wantDone: true,
			verify: func(t *testing.T, c checkout) {
				if got := mustRead(t, filepath.Join(c.Dir, "a")); string(got) != "a" {
					t.Fatalf("file a = %q; the redo must restore the checked-out content", got)
				}
			},
		},
		{
			name: "foreign repository fails and is left untouched",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				run(t, c.Dir, "init", "-q")
				return agentCommit(t, c.Dir)
			},
			wantErr: errForeignGit, wantHead: "agent",
		},
		{
			name: "marker removed by the agent never deletes the repository",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				complete(t, g, *c)
				head := agentCommit(t, c.Dir)
				if err := os.RemoveAll(c.StateDir); err != nil {
					t.Fatal(err)
				}
				return head
			},
			wantErr: errForeignGit, wantHead: "agent",
		},
		{
			name: "only the done marker removed never deletes the repository",
			setup: func(t *testing.T, g *gitRunner, c *checkout) string {
				complete(t, g, *c)
				head := agentCommit(t, c.Dir)
				if err := os.Remove(filepath.Join(c.StateDir, doneName)); err != nil {
					t.Fatal(err)
				}
				return head
			},
			wantErr: errForeignGit, wantHead: "agent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := newRunner(t)
			c := checkout{Dir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), Repo: url, Ref: "main", Depth: 1}
			agentHead := test.setup(t, g, &c)
			err := g.checkout(context.Background(), c)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			want := sha
			if test.wantHead == "agent" {
				want = agentHead
			}
			if head := run(t, c.Dir, "rev-parse", "HEAD"); head != want {
				t.Fatalf("HEAD = %s, want %s (%s)", head, want, test.wantHead)
			}
			if got := bootstrapped(c.StateDir); got != test.wantDone {
				t.Fatalf("done marker = %v, want %v", got, test.wantDone)
			}
			if test.wantErr == nil && (markers{c.StateDir}).started() {
				t.Fatal("started marker must be cleared once done")
			}
			if test.wantErr != nil {
				message := failureMessage(Request{Repo: c.Repo}, err)
				if !strings.Contains(message, "did not create") || len(message) >= 512 {
					t.Fatalf("message = %q", message)
				}
			}
			if test.verify != nil {
				test.verify(t, c)
			}
		})
	}
}

func TestCheckoutFailureLeavesStartedMarker(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "nope", Depth: 1}); !errors.Is(err, errRefNotFound) {
		t.Fatalf("error = %v, want %v", err, errRefNotFound)
	}
	if !(markers{state}).started() || bootstrapped(state) {
		t.Fatal("a failed attempt must leave started without done so the next turn redoes it")
	}
	// The retry resets our own partial .git and succeeds.
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "main", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if !bootstrapped(state) {
		t.Fatal("done marker missing after the retry")
	}
}

func TestCheckoutRefusesBadNamesBeforeTouchingTheWorkspace(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	run(t, dir, "init", "-q")
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "a..b", Depth: 1}); err == nil {
		t.Fatal("expected an error")
	}
	if (markers{state}).started() || bootstrapped(state) {
		t.Fatal("a rejected request must not write markers")
	}
}

func TestPlan(t *testing.T) {
	for _, test := range []struct {
		name                  string
		done, started, hasGit bool
		want                  action
	}{
		{name: "empty", want: actionClone},
		{name: "foreign git", hasGit: true, want: actionForeign},
		{name: "interrupted", started: true, hasGit: true, want: actionReset},
		{name: "interrupted before init", started: true, want: actionReset},
		{name: "done", done: true, hasGit: true, want: actionNone},
		{name: "done, git removed by the agent", done: true, want: actionNone},
		{name: "both markers read as done", done: true, started: true, hasGit: true, want: actionNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := plan(test.done, test.started, test.hasGit); got != test.want {
				t.Fatalf("plan = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCheckoutFailures(t *testing.T) {
	url, _ := originRepo(t)
	tests := []struct {
		name    string
		spec    checkout
		wantErr error
		want    string
	}{
		{name: "missing ref", spec: checkout{Repo: url, Ref: "nope", Depth: 1}, wantErr: errRefNotFound, want: `ref "nope" was not found`},
		{name: "missing sha", spec: checkout{Repo: url, Ref: strings.Repeat("a", 40), Depth: 1}, want: "git fetch failed"},
		{name: "option-like ref", spec: checkout{Repo: url, Ref: "--upload-pack=x", Depth: 1}, want: "ref or branch name is invalid"},
		{name: "bad branch", spec: checkout{Repo: url, Ref: "main", Branch: "a..b", Depth: 1}, want: "ref or branch name is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
			test.spec.Dir, test.spec.StateDir = dir, state
			err := g.checkout(context.Background(), test.spec)
			if err == nil {
				t.Fatal("expected an error")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			message := failureMessage(Request{Repo: "https://github.com/o/r", Ref: test.spec.Ref}, err)
			if !strings.Contains(message, test.want) {
				t.Fatalf("message %q does not contain %q", message, test.want)
			}
			if len(message) >= 512 {
				t.Fatalf("message is %d bytes; the executor replaces messages of 512 or more", len(message))
			}
			if bootstrapped(state) {
				t.Fatal("a failed checkout must not write the marker")
			}
		})
	}
}

type fakeSource struct {
	request *Request
	err     error
	calls   int
}

func (f *fakeSource) Workspace(context.Context) (*Request, error) {
	f.calls++
	return f.request, f.err
}

type fakeRunner struct{ calls int }

func (f *fakeRunner) Run(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
	f.calls++
	return runtime.Outcome{}, nil
}

func newBootstrapper(t *testing.T, source Source, next Runner) *Bootstrapper {
	t.Helper()
	b, err := New(next, Config{Dir: filepath.Join(t.TempDir(), "workspace"), StateDir: filepath.Join(t.TempDir(), "state"), Policy: apiworkspace.Git{Origins: []string{"github.com"}}, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunWithoutWorkspaceDelegatesAndAsksOnce(t *testing.T) {
	source, next := &fakeSource{}, &fakeRunner{}
	b := newBootstrapper(t, source, next)
	for range 3 {
		outcome, err := b.Run(context.Background(), runtime.Turn{}, nil)
		if err != nil || outcome.Failure != nil {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
	}
	if next.calls != 3 || source.calls != 1 {
		t.Fatalf("runner calls = %d, source calls = %d; want 3 and 1", next.calls, source.calls)
	}
}

func TestRunAfterMarkerSkipsControlPlane(t *testing.T) {
	source, next := &fakeSource{err: errors.New("down")}, &fakeRunner{}
	b := newBootstrapper(t, source, next)
	if err := (markers{b.stateDir}).markDone(context.Background(), checkout{Repo: "https://github.com/o/r"}); err != nil {
		t.Fatal(err)
	}
	outcome, _ := b.Run(context.Background(), runtime.Turn{}, nil)
	if outcome.Failure != nil || next.calls != 1 || source.calls != 0 {
		t.Fatalf("outcome = %+v, runner calls = %d, source calls = %d", outcome, next.calls, source.calls)
	}
}

func TestRunFailuresDoNotReachTheAgent(t *testing.T) {
	tests := []struct {
		name    string
		source  *fakeSource
		want    string
		retried bool
	}{
		{name: "control plane unavailable", source: &fakeSource{err: errors.New("unavailable")}, want: "could not read the workspace request"},
		{name: "host outside origins", source: &fakeSource{request: &Request{Repo: "https://gitlab.com/o/r"}}, want: "gitlab.com is not one of this agent's Git origins"},
		{name: "invalid repo", source: &fakeSource{request: &Request{Repo: "https://user:tok@github.com/o/r"}}, want: "repository URL is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := &fakeRunner{}
			b := newBootstrapper(t, test.source, next)
			for range 2 {
				outcome, err := b.Run(context.Background(), runtime.Turn{}, nil)
				if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, test.want) {
					t.Fatalf("outcome = %+v, err = %v; want failure containing %q", outcome, err, test.want)
				}
			}
			if next.calls != 0 {
				t.Fatal("the agent must not run when bootstrap fails")
			}
			if test.source.calls != 2 {
				t.Fatalf("source calls = %d; a failed bootstrap must be retried on the next turn", test.source.calls)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	good := Config{Dir: "/data/workspace", StateDir: "/data/.kagent", Policy: apiworkspace.Git{Origins: []string{"github.com"}}, Source: &fakeSource{}}
	for name, mutate := range map[string]func(*Config){
		"relative dir":           func(c *Config) { c.Dir = "workspace" },
		"relative state dir":     func(c *Config) { c.StateDir = ".kagent" },
		"no state dir":           func(c *Config) { c.StateDir = "" },
		"state dir in workspace": func(c *Config) { c.StateDir = "/data/workspace/.kagent" },
		"state dir is workspace": func(c *Config) { c.StateDir = "/data/workspace" },
		"no source":              func(c *Config) { c.Source = nil },
		"no origins":             func(c *Config) { c.Policy = apiworkspace.Git{} },
		"credential, 2 hosts":    func(c *Config) { c.Policy = apiworkspace.Git{Origins: []string{"a.com", "b.com"}, Credential: true} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mutate(&cfg)
			if _, err := New(&fakeRunner{}, cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunWithForeignGitFailsWithoutDeletingIt(t *testing.T) {
	next := &fakeRunner{}
	b := newBootstrapper(t, &fakeSource{request: &Request{Repo: "https://github.com/o/r"}}, next)
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, b.dir, "init", "-q")
	head := agentCommit(t, b.dir)
	for range 2 {
		outcome, err := b.Run(context.Background(), runtime.Turn{}, nil)
		if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "did not create") {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
	}
	if next.calls != 0 {
		t.Fatal("the agent must not run when bootstrap fails")
	}
	if got := run(t, b.dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
}

func TestMarkDoneSucceedsWhenStartedCannotBeRemoved(t *testing.T) {
	m := markers{t.TempDir()}
	// A non-empty directory in place of started makes os.Remove fail.
	stuck := filepath.Join(m.dir, startedName)
	if err := os.MkdirAll(filepath.Join(stuck, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctx := logging.IntoContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err := m.markDone(ctx, checkout{Repo: "https://github.com/o/r"}); err != nil {
		t.Fatalf("markDone = %v, want nil: done is durable, so a stuck started marker is not a failure", err)
	}
	if !m.done() {
		t.Fatal("done marker missing")
	}
	if out := logs.String(); !strings.Contains(out, "failed to remove stale bootstrap start marker") || !strings.Contains(out, stuck) {
		t.Fatalf("log = %q, want a warning naming %s", out, stuck)
	}
}

func TestClearStaleStaysQuietWhenStartedIsAlreadyGone(t *testing.T) {
	m := markers{t.TempDir()}
	if err := m.markDone(context.Background(), checkout{Repo: "https://github.com/o/r"}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m.clearStale(logging.IntoContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil))))
	if logs.Len() != 0 {
		t.Fatalf("log = %q, want none for a missing started marker", logs.String())
	}
}

func TestRunClearsStartedLeftBesideDone(t *testing.T) {
	next := &fakeRunner{}
	source := &fakeSource{err: errors.New("down")}
	b := newBootstrapper(t, source, next)
	m := markers{b.stateDir}
	c := checkout{Repo: "https://github.com/o/r"}
	if err := m.markDone(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// What a crash between writing done and removing started leaves behind.
	if err := m.markStarted(c); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.Run(context.Background(), runtime.Turn{}, nil)
	if err != nil || outcome.Failure != nil || next.calls != 1 || source.calls != 0 {
		t.Fatalf("outcome = %+v, err = %v, runner calls = %d, source calls = %d", outcome, err, next.calls, source.calls)
	}
	if m.started() || !m.done() {
		t.Fatalf("started = %v, done = %v; want the leftover started removed and done kept", m.started(), m.done())
	}
}

// With done and a leftover started, removing done later must not turn the
// agent's own .git into a reset target: started is already gone, so the .git is
// foreign and stays untouched.
func TestRunLeftoverStartedDoesNotLetLaterDoneRemovalResetAgentGit(t *testing.T) {
	next := &fakeRunner{}
	b := newBootstrapper(t, &fakeSource{request: &Request{Repo: "https://github.com/o/r"}}, next)
	m := markers{b.stateDir}
	c := checkout{Repo: "https://github.com/o/r"}
	if err := m.markDone(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := m.markStarted(c); err != nil {
		t.Fatal(err)
	}
	if outcome, err := b.Run(context.Background(), runtime.Turn{}, nil); err != nil || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
	// The agent works after the bootstrap, then done goes missing.
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, b.dir, "init", "-q")
	head := agentCommit(t, b.dir)
	if err := os.Remove(filepath.Join(b.stateDir, doneName)); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.Run(context.Background(), runtime.Turn{}, nil)
	if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "did not create") {
		t.Fatalf("outcome = %+v, err = %v; want the foreign-git failure", outcome, err)
	}
	if got := run(t, b.dir, "rev-parse", "HEAD"); got != head || !exists(filepath.Join(b.dir, "agent-work")) {
		t.Fatalf("HEAD = %s, want %s with the agent's file kept", got, head)
	}
}
