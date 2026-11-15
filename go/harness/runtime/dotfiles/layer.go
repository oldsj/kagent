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
// them, and the next turn tries again.
//
// Each process lays the files out again from the pinned bundle the first time
// it runs a turn, so edits made in an earlier run never persist, and again
// whenever an entry has gone missing. A fork has no delivery of its own; it
// lays out its source's bundle, which its restored data holds.
type Layer struct {
	next     Runner
	source   Source
	target   Target
	stateDir string
	client   *http.Client

	mu      sync.Mutex
	none    bool
	files   []File
	digest  string
	entries []string
	laid    bool
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

// Run lays out the dotfiles if needed, then runs the turn.
func (l *Layer) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	l.Ready(ctx)
	return l.next.Run(ctx, turn, sink)
}

// Ready lays out the dotfiles for a runtime that starts native work outside a
// turn. It never fails.
func (l *Layer) Ready(ctx context.Context) {
	if err := l.ensure(ctx); err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "agent dotfiles were not laid out; the turn runs without them", "error", err)
	}
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

func (l *Layer) ensure(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.none || (l.laid && intact(l.target.Dir, l.entries)) {
		return nil
	}
	if l.files == nil {
		ctx, cancel := context.WithTimeout(ctx, Timeout)
		defer cancel()
		digest, files, err := l.load(ctx)
		if err != nil {
			return err
		}
		if files == nil {
			l.none = true
			return nil
		}
		l.digest, l.files = digest, files
	}
	entries, err := lay(l.target, l.stateDir, l.digest, l.files)
	if err != nil {
		return err
	}
	l.entries, l.laid = entries, true
	return nil
}

// load returns the pinned bundle's files: from the verified copy on disk when
// its digest matches, else downloaded. Without a delivery, the bundle of the
// last layout is the pin, if one was laid out.
func (l *Layer) load(ctx context.Context) (string, []File, error) {
	delivery, err := l.source.Dotfiles(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("read the dotfiles delivery: %w", err)
	}
	digest := delivery.GetDigest()
	if delivery == nil {
		digest = readManifest(l.stateDir).Digest
	}
	if digest == "" {
		return "", nil, nil
	}
	cache := filepath.Join(l.stateDir, "bundle.tar")
	if cached, err := os.ReadFile(cache); err == nil {
		if files, err := Unpack(cached, digest); err == nil {
			return digest, files, nil
		}
	}
	if delivery == nil {
		return "", nil, nil
	}
	bundle, err := l.fetch(ctx, delivery)
	if err != nil {
		return "", nil, err
	}
	files, err := Unpack(bundle, digest)
	if err != nil {
		return "", nil, err
	}
	if err := utils.EnsurePrivateDir(l.stateDir); err != nil {
		return "", nil, err
	}
	if err := utils.ReplacePrivateFile(cache, bundle); err != nil {
		return "", nil, err
	}
	return digest, files, nil
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
