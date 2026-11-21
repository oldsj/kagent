package session

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

const (
	// crashRecoveryScanLimit bounds the admitted recoveries one pass retries.
	crashRecoveryScanLimit = 20
	// crashRecoveryRetryMin and crashRecoveryRetryMax bound the per-Session delay
	// after a failed retry, which doubles from the first to the second.
	crashRecoveryRetryMin = 15 * time.Second
	crashRecoveryRetryMax = 10 * time.Minute
)

// crashRecoveryRetries is one replica's backoff for admitted crash recoveries
// that failed. It is not durable: a restarted replica retries each pending
// recovery once at once, then backs off again. Operation claims, not this
// state, keep replicas from issuing the same revert twice.
type crashRecoveryRetries struct {
	now     func() time.Time
	pending map[string]*crashRecoveryRetry
}

type crashRecoveryRetry struct {
	due   time.Time
	delay time.Duration
	// unrecoverable is a revert that can never succeed: the recorded Actor is
	// missing or was replaced. Only deleting the Session ends it.
	unrecoverable bool
}

func newCrashRecoveryRetries(now func() time.Time) *crashRecoveryRetries {
	return &crashRecoveryRetries{now: now, pending: map[string]*crashRecoveryRetry{}}
}

// failed records a failed retry of sessionID and returns when it is due again.
func (r *crashRecoveryRetries) failed(sessionID string) time.Time {
	retry := r.pending[sessionID]
	if retry == nil {
		retry = &crashRecoveryRetry{}
		r.pending[sessionID] = retry
	}
	retry.delay = min(max(2*retry.delay, crashRecoveryRetryMin), crashRecoveryRetryMax)
	retry.due = r.now().Add(retry.delay)
	return retry.due
}

func (r *crashRecoveryRetries) due(sessionID string) bool {
	retry := r.pending[sessionID]
	return retry == nil || (!retry.unrecoverable && !r.now().Before(retry.due))
}

// retryCrashRecoveries runs one pass over admitted crash recoveries that no
// attempt holds. The idle worker issues a recovery's first revert; when that
// fails, for example because the Actor's node is still restarting, the Session
// would otherwise stay FAILED until a client calls RevertSession. Each retry
// joins the admitted operation exactly as RevertSession does and never admits
// a new one, so it reverts only an Actor Substrate confirmed crashed.
func (w *ActorWorkflow) retryCrashRecoveries(ctx context.Context, retries *crashRecoveryRetries) {
	logger := logging.FromContext(ctx)
	ids, err := w.store.ListPendingCrashRecoveries(ctx, crashRecoveryScanLimit)
	if err != nil {
		if ctx.Err() == nil {
			logger.ErrorContext(ctx, "list pending crash recoveries", "error", err)
		}
		return
	}
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
		if !retries.due(id) {
			continue
		}
		_, err := w.run(ctx, id, apiv1.RuntimeOperation_RUNTIME_OPERATION_REVERT, false)
		switch {
		case err == nil:
			delete(retries.pending, id)
			logger.InfoContext(ctx, "recovered crashed runtime in the background", "session_id", id)
		case ctx.Err() != nil:
			return
		case errors.Is(err, database.ErrFailedPrecondition):
			retries.failed(id)
			retries.pending[id].unrecoverable = true
			logger.WarnContext(ctx, "crashed runtime cannot be recovered; delete the Session", "session_id", id, "error", err)
		case errors.Is(err, database.ErrConflict), errors.Is(err, database.ErrNotFound):
			// Finished, superseded by a delete, or deleted since the scan.
			delete(retries.pending, id)
		default:
			due := retries.failed(id)
			logger.ErrorContext(ctx, "retry crash recovery", "session_id", id, "error", err, "next_attempt", due)
		}
	}
	for id := range retries.pending {
		if !listed[id] {
			delete(retries.pending, id)
		}
	}
}

// runCrashRecoveryRetries retries admitted crash recoveries every interval
// until ctx ends.
func (w *ActorWorkflow) runCrashRecoveryRetries(ctx context.Context, interval time.Duration) {
	retries := newCrashRecoveryRetries(time.Now)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		w.retryCrashRecoveries(ctx, retries)
		timer.Reset(interval)
	}
}
