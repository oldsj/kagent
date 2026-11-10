// Package activity reports whether a Session runtime is in use outside A2A
// tasks, such as by an attached client, so the control plane can adopt wakes
// it did not issue and suspend the runtime once nothing uses it.
package activity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sender makes one activity report and returns when an active runtime should
// report again. taskstore.Store implements it.
type Sender interface {
	ReportActivity(ctx context.Context, active bool) (renewAfter time.Duration, err error)
}

const (
	// defaultRenewAfter applies when the control plane names no interval.
	defaultRenewAfter = 20 * time.Second
	minRetry          = time.Second
	maxRetry          = 30 * time.Second
	// deniedRetries bounds PermissionDenied retries of an active report.
	deniedRetries = 3
)

// Reporter turns busy/idle transitions into activity reports. It sends
// nothing until the first SetActive, so a runtime never reports during image
// warm-up or golden-snapshot creation. While active it renews at the interval
// the control plane returns. Transient failures retry with backoff.
// PermissionDenied means the control plane does not observe this Actor
// running for the Session. Just after a wake that can be a brief lag, so an
// active report retries it a few times; after that the Actor is taken to be
// suspending, replaced or deleted, and reporting stops until the next
// busy/idle transition.
type Reporter struct {
	sender  Sender
	logger  *slog.Logger
	changed chan struct{}
	// Retry backoff bounds; tests shorten them.
	retryMin, retryMax time.Duration

	mu      sync.Mutex
	set     bool
	active  bool
	version uint64
}

func NewReporter(sender Sender, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{sender: sender, logger: logger, changed: make(chan struct{}, 1), retryMin: minRetry, retryMax: maxRetry}
}

// SetActive records whether anything uses the runtime. It never blocks and
// deduplicates repeated values; call it on every busy/idle change.
func (r *Reporter) SetActive(active bool) {
	r.mu.Lock()
	if r.set && r.active == active {
		r.mu.Unlock()
		return
	}
	r.set, r.active = true, active
	r.version++
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *Reporter) desired() (bool, bool, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set, r.active, r.version
}

// Run sends reports until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	var timer *time.Timer
	var wake <-chan time.Time
	retry := r.retryMin
	denied := 0
	// sent is the version last delivered; done means nothing is owed until
	// the next renewal or change.
	var sent uint64
	done := true
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-r.changed:
			done = false
		case <-wake:
			done = false
		}
		set, active, version := r.desired()
		if !set || (done && version == sent) {
			continue
		}
		if timer != nil {
			timer.Stop()
		}
		wake = nil
		renewAfter, err := r.sender.ReportActivity(ctx, active)
		switch {
		case err == nil:
			sent, done, retry, denied = version, true, r.retryMin, 0
			if active {
				if renewAfter <= 0 {
					renewAfter = defaultRenewAfter
				}
				timer = time.NewTimer(renewAfter)
				wake = timer.C
			}
		case status.Code(err) == codes.PermissionDenied && (!active || denied >= deniedRetries):
			// Only a later busy/idle transition reports again.
			r.logger.InfoContext(ctx, "runtime activity not accepted; this Actor does not serve the Session", "active", active)
			sent, done, retry, denied = version, true, r.retryMin, 0
		case ctx.Err() != nil:
			return
		default:
			if status.Code(err) == codes.PermissionDenied {
				denied++
			}
			r.logger.WarnContext(ctx, "report runtime activity", "active", active, "error", err, "retry_after", retry)
			timer = time.NewTimer(retry)
			wake = timer.C
			retry = min(2*retry, r.retryMax)
		}
	}
}
