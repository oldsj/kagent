package workspace

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/stretchr/testify/require"
)

type proxyRequest struct{ host, path, query, authorization string }
type gitProxyFixture struct {
	server                             *httptest.Server
	bare, sha                          string
	available, pushAvailable, redirect atomic.Bool
	mu                                 sync.Mutex
	requests                           []proxyRequest
	receives                           int
}

func newGitProxyFixture(t *testing.T) *gitProxyFixture {
	t.Helper()
	raw, sha := originRepo(t)
	f := &gitProxyFixture{bare: strings.TrimPrefix(raw, "file://"), sha: sha}
	run(t, f.bare, "config", "http.receivepack", "true")
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(f.bare), "GIT_HTTP_EXPORT_ALL=1"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, proxyRequest{r.URL.Host, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")})
		f.mu.Unlock()
		readHost := "mainloop-git-read.mainloop.svc.cluster.local"
		pushHost := "mainloop-git-push.mainloop.svc.cluster.local"
		if r.Method == http.MethodConnect || (r.URL.Host != readHost && r.URL.Host != pushHost) || !strings.HasPrefix(r.URL.Path, "/owner/repo.git/") {
			http.Error(w, "denied authority", http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != strings.TrimPrefix(placeholderHeader, "Authorization: ") {
			http.Error(w, "missing placeholder", http.StatusForbidden)
			return
		}
		if !f.available.Load() {
			http.Error(w, "capability not published", http.StatusForbidden)
			return
		}
		if f.redirect.Load() {
			http.Redirect(w, r, "https://github.com/owner/repo.git/info/refs", http.StatusFound)
			return
		}
		receive := strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
		if receive && r.URL.Host == readHost {
			http.Error(w, "read listener refuses writes", http.StatusForbidden)
			return
		}
		if receive && !f.pushAvailable.Load() {
			http.Error(w, "push capability not published", http.StatusForbidden)
			return
		}
		if receive {
			f.mu.Lock()
			f.receives++
			f.mu.Unlock()
		}
		r.URL.Path = "/origin.git/" + strings.TrimPrefix(r.URL.Path, "/owner/repo.git/")
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RequestURI = r.URL.RequestURI()
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *gitProxyFixture) environment() []string {
	return append(append([]string{"PATH=" + os.Getenv("PATH"), "http_proxy=" + f.server.URL, "HTTP_PROXY=" + f.server.URL, "https_proxy=" + f.server.URL, "HTTPS_PROXY=" + f.server.URL, "ALL_PROXY=" + f.server.URL, "all_proxy=" + f.server.URL, "NO_PROXY=", "no_proxy="}, gitIdentity...), "GIT_CONFIG_COUNT=0")
}
func (f *gitProxyFixture) snapshot() ([]proxyRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proxyRequest(nil), f.requests...), f.receives
}
func proxyBootstrapper(t *testing.T, f *gitProxyFixture, push bool, request *Request) (*Bootstrapper, *fakeSource, *fakeRunner) {
	t.Helper()
	source, next := &fakeSource{request: request}, &fakeRunner{}
	policy := apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin)}
	if push {
		policy.PushProxyOrigin = new(apiworkspace.PushProxyOrigin)
	}
	b, err := New(next, Config{Dir: filepath.Join(t.TempDir(), "workspace"), StateDir: t.TempDir(), Policy: policy, Source: source, Environment: f.environment()})
	require.NoError(t, err)
	return b, source, next
}
func TestProxyBootstrapDeferredFailureRetryAndResume(t *testing.T) {
	f := newGitProxyFixture(t)
	canonical := "https://GitHub.com/Owner/Repo.git"
	b, source, next := proxyBootstrapper(t, f, true, &Request{Repo: canonical, Ref: "main", Branch: "Feature-Case", Depth: 1})
	requests, _ := f.snapshot()
	require.Empty(t, requests)
	require.Zero(t, source.calls, "construction cannot resolve workspace/capability")
	out, err := b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.NotNil(t, out.Failure)
	require.Zero(t, next.calls)
	require.True(t, markers{b.stateDir}.started())
	require.False(t, bootstrapped(b.stateDir))
	requests, _ = f.snapshot()
	require.NotEmpty(t, requests)
	for _, r := range requests {
		require.Equal(t, "mainloop-git-read.mainloop.svc.cluster.local", r.host)
		require.Equal(t, strings.TrimPrefix(placeholderHeader, "Authorization: "), r.authorization)
	}
	// Publication changes only the controlled listener, not policy, refs or identity.
	f.available.Store(true)
	out, err = b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.Nil(t, out.Failure)
	require.Equal(t, 1, next.calls)
	require.Equal(t, 2, source.calls)
	require.True(t, bootstrapped(b.stateDir))
	require.False(t, markers{b.stateDir}.started())
	require.Equal(t, f.sha, run(t, b.dir, "rev-parse", "HEAD"))
	require.Equal(t, "Feature-Case", run(t, b.dir, "branch", "--show-current"))
	require.Equal(t, "1", run(t, b.dir, "rev-list", "--count", "HEAD"))
	require.Equal(t, apiworkspace.ReadProxyOrigin+"/owner/repo.git", run(t, b.dir, "remote", "get-url", "origin"))
	require.Equal(t, apiworkspace.PushProxyOrigin+"/owner/repo.git", run(t, b.dir, "remote", "get-url", "--push", "origin"))
	for _, origin := range []string{apiworkspace.ReadProxyOrigin, apiworkspace.PushProxyOrigin} {
		require.Equal(t, placeholderHeader, run(t, b.dir, "config", "--local", "--get", "http."+origin+"/.extraHeader"))
	}
	require.Equal(t, "false", run(t, b.dir, "config", "--local", "--get", "http.followRedirects"))
	require.Contains(t, string(mustRead(t, filepath.Join(b.stateDir, doneName))), "repo="+canonical)
	head := agentCommit(t, b.dir)
	f.pushAvailable.Store(true)
	_, err = b.git.git(t.Context(), b.dir, "push", "push", "origin", "HEAD:refs/heads/feature-owned")
	require.NoError(t, err)
	require.Equal(t, head, run(t, f.bare, "rev-parse", "refs/heads/feature-owned"))
	requests, receives := f.snapshot()
	require.Positive(t, receives)
	var sawPush bool
	for _, r := range requests {
		if r.host == "mainloop-git-push.mainloop.svc.cluster.local" {
			sawPush = true
			require.Equal(t, strings.TrimPrefix(placeholderHeader, "Authorization: "), r.authorization)
		}
	}
	require.True(t, sawPush)
	// A restarted bootstrap honors done and preserves the agent's checkout.
	resumed, err := New(next, Config{Dir: b.dir, StateDir: b.stateDir, Policy: b.policy, Source: &fakeSource{err: context.Canceled}, Environment: f.environment()})
	require.NoError(t, err)
	f.available.Store(false)
	out, err = resumed.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.Nil(t, out.Failure)
	after, _ := f.snapshot()
	require.Len(t, after, len(requests))
	require.Equal(t, head, run(t, b.dir, "rev-parse", "HEAD"))
}
func TestProxyReadOnlyAndRedirectsFailClosed(t *testing.T) {
	for _, name := range []string{"readonly push", "unpublished push", "redirect", "invalid identity", "foreign Git"} {
		t.Run(name, func(t *testing.T) {
			f := newGitProxyFixture(t)
			f.available.Store(true)
			b, _, next := proxyBootstrapper(t, f, name == "unpublished push", &Request{Repo: "https://github.com/Owner/Repo", Ref: "main"})
			switch name {
			case "redirect":
				f.redirect.Store(true)
			case "invalid identity":
				b.source = &fakeSource{request: &Request{Repo: "https://github.com/o/%2fr"}}
			case "foreign Git":
				require.NoError(t, os.MkdirAll(b.dir, 0700))
				run(t, b.dir, "init", "-q")
				agentCommit(t, b.dir)
			}
			out, err := b.Run(t.Context(), runtime.Turn{}, nil)
			require.NoError(t, err)
			if name != "readonly push" && name != "unpublished push" {
				require.NotNil(t, out.Failure)
				require.Zero(t, next.calls)
				require.False(t, bootstrapped(b.stateDir))
				requests, _ := f.snapshot()
				for _, r := range requests {
					require.Equal(t, "mainloop-git-read.mainloop.svc.cluster.local", r.host)
				}
				if name == "invalid identity" || name == "foreign Git" {
					require.Empty(t, requests)
				}
				return
			}
			require.Nil(t, out.Failure)
			agentCommit(t, b.dir)
			_, err = b.git.git(t.Context(), b.dir, "push", "push", "origin", "HEAD:refs/heads/denied")
			require.Error(t, err)
			_, receives := f.snapshot()
			require.Zero(t, receives)
			origin := apiworkspace.ReadProxyOrigin
			if name == "unpublished push" {
				origin = apiworkspace.PushProxyOrigin
			}
			require.Equal(t, origin+"/owner/repo.git", run(t, b.dir, "remote", "get-url", "--push", "origin"))
		})
	}
}
func TestProxyRefModesUseRealGit(t *testing.T) {
	f := newGitProxyFixture(t)
	f.available.Store(true)
	for _, ref := range []string{"", "dev", "v1", f.sha} {
		t.Run(ref, func(t *testing.T) {
			b, _, _ := proxyBootstrapper(t, f, false, &Request{Repo: "https://github.com/Owner/Repo", Ref: ref, Depth: 2})
			out, err := b.Run(t.Context(), runtime.Turn{}, nil)
			require.NoError(t, err)
			require.Nil(t, out.Failure)
			if ref == "" || ref == f.sha || ref == "v1" {
				require.Equal(t, f.sha, run(t, b.dir, "rev-parse", "HEAD"))
			}
			if ref == "v1" || ref == f.sha {
				require.Empty(t, run(t, b.dir, "branch", "--show-current"))
			}
		})
	}
}
