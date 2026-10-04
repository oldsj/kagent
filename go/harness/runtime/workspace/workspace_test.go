package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/runtime"
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
			g, dir := newRunner(t), t.TempDir()
			err := g.checkout(context.Background(), checkout{Dir: dir, Repo: url, Ref: test.ref, Branch: test.branch, Depth: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !bootstrapped(dir) {
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
	g, dir := newRunner(t), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, Repo: url, Ref: "main", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if count := run(t, dir, "rev-list", "--count", "HEAD"); count != "1" {
		t.Fatalf("commit count = %s, want 1", count)
	}
}

func TestCheckoutPlaceholderHeaderIsRepoLocal(t *testing.T) {
	url, _ := originRepo(t)
	g, dir := newRunner(t), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, Repo: url, Ref: "main", Depth: 1, Host: "github.com", Credential: true}); err != nil {
		t.Fatal(err)
	}
	got := run(t, dir, "config", "--local", "--get", "http.https://github.com/.extraHeader")
	if got != placeholderHeader {
		t.Fatalf("extraHeader = %q", got)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(dir, ".git", markerName))), "Basic") {
		t.Fatal("marker must not contain credentials")
	}
}

func TestCheckoutWithoutCredentialSendsNoHeader(t *testing.T) {
	url, _ := originRepo(t)
	g, dir := newRunner(t), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, Repo: url, Ref: "main", Depth: 1, Host: "github.com"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "--local", "--get-regexp", "extraheader")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("anonymous checkout configured a header: %s", out)
	}
}

func TestCheckoutRecoversPartialClone(t *testing.T) {
	url, _ := originRepo(t)
	g, dir := newRunner(t), t.TempDir()
	// An interrupted attempt: .git exists, a stale ref lock sits in it, no marker.
	run(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.checkout(context.Background(), checkout{Dir: dir, Repo: url, Ref: "main", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if !bootstrapped(dir) {
		t.Fatal("marker missing after recovery")
	}
	if got := mustRead(t, filepath.Join(dir, "scratch")); string(got) != "keep" {
		t.Fatal("recovery must leave unrelated files alone")
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
			g, dir := newRunner(t), t.TempDir()
			test.spec.Dir = dir
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
			if bootstrapped(dir) {
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
	b, err := New(next, Config{Dir: filepath.Join(t.TempDir(), "workspace"), Policy: apiworkspace.Git{Origins: []string{"github.com"}}, Source: source})
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
	if err := os.MkdirAll(filepath.Join(b.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(b.dir), nil, 0o644); err != nil {
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
	good := Config{Dir: "/data/workspace", Policy: apiworkspace.Git{Origins: []string{"github.com"}}, Source: &fakeSource{}}
	for name, mutate := range map[string]func(*Config){
		"relative dir":        func(c *Config) { c.Dir = "workspace" },
		"no source":           func(c *Config) { c.Source = nil },
		"no origins":          func(c *Config) { c.Policy = apiworkspace.Git{} },
		"credential, 2 hosts": func(c *Config) { c.Policy = apiworkspace.Git{Origins: []string{"a.com", "b.com"}, Credential: true} },
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
