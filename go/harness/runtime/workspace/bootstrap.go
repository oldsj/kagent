// Package workspace checks out a Session's requested Git repository before the
// first turn of a Harness runtime. It wraps a runner so every adapter shares one
// bootstrap, and it turns bootstrap problems into clear task failures instead
// of leaving the agent to discover an empty or half-cloned directory.
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// Timeout bounds one bootstrap attempt, including every git invocation.
const Timeout = 5 * time.Minute

// Request is the checkout a Session asked for.
type Request struct {
	Repo   string
	Ref    string
	Branch string
	// Depth is the shallow-clone depth; zero means apiworkspace.DefaultDepth.
	Depth int
}

// Source reads the Session's workspace request from the control plane. A nil
// Request means the Session asked for none.
type Source interface {
	Workspace(context.Context) (*Request, error)
}

// Runner is the turn-execution capability the bootstrap wraps.
type Runner interface {
	Run(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error)
}

// Bootstrapper clones a requested repository into Dir before delegating a turn.
type Bootstrapper struct {
	next     Runner
	source   Source
	policy   apiworkspace.Git
	git      *gitRunner
	dir      string
	stateDir string

	mu sync.Mutex
	// none caches that the Session asked for no workspace, so later turns of this
	// process skip the control-plane call. A resumed actor asks again once.
	none bool
}

// Config configures New.
type Config struct {
	// Dir is the workspace directory, normally /data/workspace on the durable disk.
	Dir string
	// StateDir holds the bootstrap markers, normally /data/.kagent on the durable
	// disk. It must be absolute and outside Dir so nothing the agent does in the
	// workspace can touch the markers.
	StateDir string
	// Policy is the compiled Git policy; requests outside its origins fail.
	Policy apiworkspace.Git
	Source Source
	// Environment is the process environment given to git.
	Environment []string
}

// New wraps next so the first turn bootstraps the workspace.
func New(next Runner, cfg Config) (*Bootstrapper, error) {
	if !filepath.IsAbs(cfg.Dir) {
		return nil, fmt.Errorf("workspace directory %q must be absolute", cfg.Dir)
	}
	if err := checkStateDir(cfg.Dir, cfg.StateDir); err != nil {
		return nil, err
	}
	if cfg.Source == nil {
		return nil, fmt.Errorf("workspace source is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	git, err := newGitRunner(cfg.Environment)
	if err != nil {
		return nil, err
	}
	return &Bootstrapper{next: next, source: cfg.Source, policy: cfg.Policy, git: git, dir: cfg.Dir, stateDir: cfg.StateDir}, nil
}

// Run bootstraps the workspace if needed, then runs the turn. A bootstrap
// failure ends the turn with a vetted message and never reaches the agent.
func (b *Bootstrapper) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if failure := b.ensure(ctx); failure != "" {
		return runtime.Outcome{Failure: &runtime.Failure{Message: failure}}, nil
	}
	return b.next.Run(ctx, turn, sink)
}

// ensure returns a public failure message, or "" when the workspace is ready or
// was never requested.
func (b *Bootstrapper) ensure(ctx context.Context) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.none {
		return ""
	}
	if bootstrapped(b.stateDir) {
		// A started marker left beside done would turn a later removal of done
		// into a reset of the agent's own .git.
		markers{b.stateDir}.clearStale(ctx)
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	request, err := b.source.Workspace(ctx)
	if err != nil {
		return "Workspace bootstrap failed: could not read the workspace request from the control plane."
	}
	if request == nil {
		b.none = true
		return ""
	}
	return b.bootstrap(ctx, *request)
}

func (b *Bootstrapper) bootstrap(ctx context.Context, request Request) string {
	host, err := apiworkspace.RepoHost(request.Repo)
	if err != nil {
		return "Workspace bootstrap failed: the repository URL is invalid."
	}
	if !b.policy.Allows(host) {
		return fmt.Sprintf("Workspace bootstrap failed: %s is not one of this agent's Git origins.", host)
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return "Workspace bootstrap failed: the workspace directory is not writable."
	}
	spec := checkout{
		Dir: b.dir, StateDir: b.stateDir, Repo: request.Repo, Host: host, Ref: request.Ref, Branch: request.Branch,
		Depth: request.Depth, Credential: b.policy.Credential,
	}
	if spec.Depth <= 0 {
		spec.Depth = apiworkspace.DefaultDepth
	}
	if err := b.git.checkout(ctx, spec); err != nil {
		return failureMessage(request, err)
	}
	return ""
}

// checkStateDir rejects a marker directory the agent could reach through the
// workspace: a relative path, or Dir itself or anything inside it.
func checkStateDir(dir, stateDir string) error {
	if !filepath.IsAbs(stateDir) {
		return fmt.Errorf("workspace state directory %q must be absolute", stateDir)
	}
	rel, err := filepath.Rel(dir, stateDir)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace state directory %q must be outside the workspace directory %q", stateDir, dir)
	}
	return nil
}
