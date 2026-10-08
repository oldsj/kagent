package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// refExists reports whether ref resolves in dir. It never fails the test, so a
// caller can assert that a ref is absent.
func refExists(dir, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

func TestCheckoutSetsDefaultBranchRef(t *testing.T) {
	url, sha := originRepo(t)
	tests := []struct {
		name       string
		ref        string
		wantBranch string
	}{
		{name: "default branch", wantBranch: "main"},
		{name: "named branch", ref: "dev", wantBranch: "dev"},
		{name: "tag", ref: "v1"},
		{name: "commit", ref: sha},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
			if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: test.ref, Depth: 1}); err != nil {
				t.Fatal(err)
			}
			if got := run(t, dir, "rev-parse", "refs/remotes/origin/main"); got != sha {
				t.Fatalf("origin/main = %s, want %s", got, sha)
			}
			if got := run(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
				t.Fatalf("origin/HEAD = %q, want refs/remotes/origin/main", got)
			}
			if branch := run(t, dir, "branch", "--show-current"); branch != test.wantBranch {
				t.Fatalf("branch = %q, want %q", branch, test.wantBranch)
			}
		})
	}
}

// The trunk uses the checkout's depth: origin/main holds exactly that many
// commits, which is the documented depth policy in fetchDefaultBranch.
func TestCheckoutDefaultBranchUsesCheckoutDepth(t *testing.T) {
	url, _ := originRepo(t)
	for _, depth := range []int{1, 2} {
		t.Run("depth "+strconv.Itoa(depth), func(t *testing.T) {
			g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
			// dev is checked out, so main is fetched only for the trunk ref.
			if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "dev", Depth: depth}); err != nil {
				t.Fatal(err)
			}
			if count := run(t, dir, "rev-list", "--count", "refs/remotes/origin/main"); count != strconv.Itoa(depth) {
				t.Fatalf("origin/main commit count = %s, want %d", count, depth)
			}
		})
	}
}

// A remote whose HEAD names no branch cannot report a default. The requested
// checkout must still succeed, and origin/HEAD must stay unset.
func TestCheckoutSucceedsWhenRemoteDefaultIsUnknown(t *testing.T) {
	url, sha := originRepo(t)
	dangling := filepath.Join(t.TempDir(), "dangling.git")
	run(t, "", "clone", "-q", "--bare", strings.TrimPrefix(url, "file://"), dangling)
	run(t, dangling, "symbolic-ref", "HEAD", "refs/heads/gone")

	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	var logs bytes.Buffer
	ctx := logging.IntoContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err := g.checkout(ctx, checkout{Dir: dir, StateDir: state, Repo: "file://" + dangling, Ref: "main", Depth: 1}); err != nil {
		t.Fatalf("checkout = %v, want nil: the default branch is not required for the bootstrap", err)
	}
	if !bootstrapped(state) {
		t.Fatal("done marker missing")
	}
	if head := run(t, dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("HEAD = %s, want %s", head, sha)
	}
	if refExists(dir, "refs/remotes/origin/HEAD") {
		t.Fatal("origin/HEAD must not be set without a default branch name")
	}
	if out := logs.String(); !strings.Contains(out, "no default branch name") {
		t.Fatalf("log = %q, want a warning about the missing default", out)
	}
}

// forkedOriginRepo builds a bare repository in which feature forks from main
// at fork, and main then moves on. It returns the file:// URL, the fork point,
// and feature's tip.
func forkedOriginRepo(t *testing.T) (url, fork, feature string) {
	t.Helper()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "origin.git")
	run(t, work, "init", "-q", "-b", "main")
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, work, "add", ".")
		run(t, work, "commit", "-q", "-m", name)
	}
	commit("a")
	commit("b")
	fork = run(t, work, "rev-parse", "HEAD")
	run(t, work, "checkout", "-q", "-b", "feature")
	commit("feature-only")
	feature = run(t, work, "rev-parse", "HEAD")
	run(t, work, "checkout", "-q", "main")
	commit("main-only")
	run(t, "", "clone", "-q", "--bare", work, bare)
	return "file://" + bare, fork, feature
}

// Depth zero means full history. A feature branch that forked before main moved
// on must still have a merge-base with origin/main, and the trunk must hold
// its whole history.
func TestCheckoutDepthZeroKeepsMergeBaseWithTrunk(t *testing.T) {
	url, fork, feature := forkedOriginRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "feature", Depth: 0}); err != nil {
		t.Fatal(err)
	}
	if shallow := run(t, dir, "rev-parse", "--is-shallow-repository"); shallow != "false" {
		t.Fatalf("depth 0 checkout is shallow = %s, want false", shallow)
	}
	if head := run(t, dir, "rev-parse", "HEAD"); head != feature {
		t.Fatalf("HEAD = %s, want %s", head, feature)
	}
	if count := run(t, dir, "rev-list", "--count", "refs/remotes/origin/main"); count != "3" {
		t.Fatalf("origin/main commit count = %s, want 3 (full history)", count)
	}
	if base := run(t, dir, "merge-base", "HEAD", "refs/remotes/origin/main"); base != fork {
		t.Fatalf("merge-base = %s, want the fork point %s", base, fork)
	}
	// The diff tools run against the trunk must work, not just the ref lookup.
	if changed := run(t, dir, "diff", "--name-only", "origin/main...HEAD"); changed != "feature-only" {
		t.Fatalf("origin/main...HEAD changed files = %q, want feature-only", changed)
	}
}

// An explicit positive depth stays shallow, as it was before depth zero meant
// full history. The feature branch then has no merge-base with the trunk: a
// documented limit of shallow clones.
func TestCheckoutExplicitDepthStaysShallow(t *testing.T) {
	url, _, _ := forkedOriginRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	if err := g.checkout(context.Background(), checkout{Dir: dir, StateDir: state, Repo: url, Ref: "feature", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if shallow := run(t, dir, "rev-parse", "--is-shallow-repository"); shallow != "true" {
		t.Fatalf("depth 1 checkout is shallow = %s, want true", shallow)
	}
	if count := run(t, dir, "rev-list", "--count", "HEAD"); count != "1" {
		t.Fatalf("HEAD commit count = %s, want 1", count)
	}
	merge := exec.Command("git", "merge-base", "HEAD", "refs/remotes/origin/main")
	merge.Dir = dir
	if err := merge.Run(); err == nil {
		t.Fatal("depth 1 feature checkout unexpectedly has a merge-base with origin/main")
	}
}

// A default branch the remote cannot serve is logged and skipped. The caller
// has already succeeded, so this must not return an error.
func TestFetchDefaultBranchFailureIsLoggedNotReturned(t *testing.T) {
	url, _ := originRepo(t)
	g, dir, state := newRunner(t), t.TempDir(), t.TempDir()
	c := checkout{Dir: dir, StateDir: state, Repo: url, Ref: "main", Depth: 1}
	if err := g.checkout(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	ctx := logging.IntoContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	g.fetchDefaultBranch(ctx, c, "missing")
	if refExists(dir, "refs/remotes/origin/missing") {
		t.Fatal("a failed default fetch must not leave its ref behind")
	}
	if got := run(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
		t.Fatalf("origin/HEAD = %q; a failed default fetch must not repoint it", got)
	}
	if out := logs.String(); !strings.Contains(out, "default branch was not fetched") || !strings.Contains(out, "missing") {
		t.Fatalf("log = %q, want a warning naming the branch", out)
	}
}
