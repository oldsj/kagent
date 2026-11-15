package dotfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	apidotfiles "github.com/kagent-dev/kagent/go/api/dotfiles"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// Timeout bounds reading the delivery and downloading the bundle.
const Timeout = 2 * time.Minute

// placeholder is the header value the egress gateway overwrites with the
// Session credential bound to the bundle url's origin. The gateway replaces a
// header only when the client sends one, and the real value never enters the
// runtime.
const placeholder = "placeholder"

// Source reads the Session's dotfiles delivery from the control plane; nil
// means the Session was created without dotfiles.
type Source interface {
	Dotfiles(context.Context) (*runtimev1.DotfilesDelivery, error)
}

// Of returns source's dotfiles view, or nil when it has none.
func Of(source any) Source {
	s, _ := source.(Source)
	return s
}

// Runner is the turn-execution capability the layer wraps.
type Runner interface {
	Run(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error)
}

// Layer lays the Session's dotfiles out before the turns of next. Dotfiles are
// context, not a precondition: a failure is logged, the turn runs without
// them (or with the ones already laid out), and the next turn tries again.
//
// Each process lays the files out again from the pinned bundle the first time
// it runs a turn, so edits made in an earlier run never persist, and again
// whenever an entry has gone missing. A fork has no delivery of its own; it
// lays out its source's bundle, which its restored data holds.
//
// Every turn reads the delivery again, so an updated Session
// (UpdateSessionDotfiles) is laid out from its next turn on, never mid-turn.
// A CLI process that outlives turns reads its configuration only at start; the
// restart hook ([Layer.OnChange]) restarts it, and stays pending until it can.
type Layer struct {
	next     Runner
	source   Source
	target   Target
	stateDir string
	client   *http.Client

	mu      sync.Mutex
	files   []File
	digest  string
	entries []string
	laid    bool
	restart func(context.Context) (bool, error)
	pending bool
}

// New wraps next. stateDir holds the verified bundle and the manifest of the
// last layout, normally <data>/.kagent/dotfiles.
func New(next Runner, source Source, target Target, stateDir string) *Layer {
	return &Layer{next: next, source: source, target: target, stateDir: stateDir, client: &http.Client{
		Timeout: Timeout,
		// The credential is for the bundle's origin only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// OnChange sets the hook that restarts a CLI process which outlives turns
// after the layer replaced its files. It reports whether it restarted (or
// nothing was running); until it has, it is called again before each turn.
// [Layer.Ready], called as that process starts, settles it.
func (l *Layer) OnChange(restart func(context.Context) (bool, error)) {
	l.mu.Lock()
	l.restart = restart
	l.mu.Unlock()
}

// Run lays out the dotfiles if needed, then runs the turn.
func (l *Layer) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	changed, err := l.ensure(ctx)
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "agent dotfiles were not laid out; the turn runs without new ones", "error", err)
	}
	l.mu.Lock()
	l.pending = l.pending || changed
	restart := l.restart
	pending := l.pending && restart != nil
	l.mu.Unlock()
	if pending {
		done, err := restart(ctx)
		if err != nil {
			logging.FromContext(ctx).WarnContext(ctx, "the agent was not restarted for its new dotfiles; it will be before a later turn", "error", err)
		}
		if done && err == nil {
			l.mu.Lock()
			l.pending = false
			l.mu.Unlock()
		}
	}
	return l.next.Run(ctx, turn, sink)
}

// Ready lays out the dotfiles for a runtime that starts native work outside a
// turn, as its CLI process starts. It never fails.
func (l *Layer) Ready(ctx context.Context) {
	if _, err := l.ensure(ctx); err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "agent dotfiles were not laid out; the agent starts without new ones", "error", err)
	}
	// The process now starting reads what is laid out.
	l.mu.Lock()
	l.pending = false
	l.mu.Unlock()
}

// RestorePending returns the wrapped runner's pending turn, as the executor
// asks the runner it was given.
func (l *Layer) RestorePending() (taskID, contextID string, pending runtime.PendingTurn, err error) {
	restorer, ok := l.next.(interface {
		RestorePending() (string, string, runtime.PendingTurn, error)
	})
	if !ok {
		return "", "", nil, nil
	}
	return restorer.RestorePending()
}

// Steer hands a message to the wrapped runner's running turn.
func (l *Layer) Steer(ctx context.Context, steer runtime.Steer) error {
	steering, ok := l.next.(runtime.Steering)
	if !ok {
		return runtime.SteerUnsupported
	}
	return steering.Steer(ctx, steer)
}

// ensure lays out the Session's current bundle. It reports whether it laid out
// another bundle than the one this process had, so a running agent has not
// read it; a hook with no agent running has nothing to restart. When the delivery cannot be read
// or the new bundle cannot be loaded, the files already laid out stay.
func (l *Layer) ensure(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	delivery, err := l.source.Dotfiles(ctx)
	if err != nil {
		return false, errors.Join(fmt.Errorf("read the dotfiles delivery: %w", err), l.keep())
	}
	digest := delivery.GetDigest()
	if delivery == nil {
		// A fork keeps the bundle of its source, which its data holds.
		digest = l.digest
		if digest == "" {
			digest = readManifest(l.stateDir).Digest
		}
	}
	if digest == "" {
		return false, nil
	}
	if digest == l.digest && l.files != nil {
		return false, l.keep()
	}
	files, err := l.load(ctx, delivery, digest)
	if err != nil {
		return false, errors.Join(err, l.keep())
	}
	if files == nil {
		return false, nil
	}
	l.digest, l.files, l.laid = digest, files, false
	return true, l.keep()
}

// keep lays the current bundle out again unless it is intact.
func (l *Layer) keep() error {
	if l.files == nil || (l.laid && intact(l.target.Dir, l.entries)) {
		return nil
	}
	entries, err := lay(l.target, l.stateDir, l.digest, l.files)
	if err != nil {
		return err
	}
	l.entries, l.laid = entries, true
	return nil
}

// load returns the files of the bundle with digest: from the verified copy on
// disk when it is that bundle, else downloaded through delivery. Without a
// delivery only the copy on disk can serve.
func (l *Layer) load(ctx context.Context, delivery *runtimev1.DotfilesDelivery, digest string) ([]File, error) {
	cache := filepath.Join(l.stateDir, "bundle.tar")
	if cached, err := os.ReadFile(cache); err == nil {
		if files, err := Unpack(cached, digest); err == nil {
			return files, nil
		}
	}
	if delivery == nil {
		return nil, nil
	}
	bundle, err := l.fetch(ctx, delivery)
	if err != nil {
		return nil, err
	}
	files, err := Unpack(bundle, digest)
	if err != nil {
		return nil, err
	}
	if err := utils.EnsurePrivateDir(l.stateDir); err != nil {
		return nil, err
	}
	if err := utils.ReplacePrivateFile(cache, bundle); err != nil {
		return nil, err
	}
	return files, nil
}

func (l *Layer) fetch(ctx context.Context, d *runtimev1.DotfilesDelivery) ([]byte, error) {
	if _, err := apidotfiles.Origin(d.GetUrl()); err != nil {
		return nil, err
	}
	if d.GetHeader() == "" {
		return nil, errors.New("dotfiles delivery names no credential header")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.GetUrl(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(d.GetHeader(), placeholder)
	response, err := l.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download the dotfiles bundle: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download the dotfiles bundle: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, apidotfiles.MaxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download the dotfiles bundle: %w", err)
	}
	if len(data) > apidotfiles.MaxBundleBytes {
		return nil, errors.New("dotfiles bundle is over the size limit")
	}
	return data, nil
}
