package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

var _ manager.Runnable = (*ActorWorkflow)(nil)
var _ manager.LeaderElectionRunnable = (*ActorWorkflow)(nil)

// Every API replica can process idle work; PostgreSQL grants each claim once.
func (*ActorWorkflow) NeedLeaderElection() bool { return false }

// wakeQuiescence is a bounded, nonblocking hint. A buffered signal survives a
// settlement racing with a worker's empty scan or a burst while all workers are
// busy. PostgreSQL, rather than the channel, holds the work to drain.
func (w *ActorWorkflow) wakeQuiescence() {
	select {
	case w.quiescenceWake <- struct{}{}:
	default:
	}
}

// Start suspends idle sessions to VOLUMES independently of task publication.
// Startup and periodic recovery scans discover work missed by local signals.
// Workers drain claims with bounded concurrency; task reads never wait for them.
func (w *ActorWorkflow) Start(ctx context.Context) error {
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			timer := time.NewTimer(w.recoveryInterval)
			defer timer.Stop()
			for ctx.Err() == nil {
				work, err := w.store.ClaimSessionQuiescence(ctx)
				if err == nil {
					// Hand off a hint before runtime I/O so a single settlement
					// burst can enlist all workers, even if this actor is slow.
					w.wakeQuiescence()
					w.quiesceIdleSession(ctx, work)
					continue
				}
				if errors.Is(err, database.ErrNotFound) {
					activity, activityErr := w.store.ClaimSessionActivityQuiescence(ctx)
					if activityErr == nil {
						w.wakeQuiescence()
						w.quiesceInactiveSession(ctx, activity)
						continue
					}
					err = activityErr
				}
				delay := min(time.Second, w.recoveryInterval)
				if errors.Is(err, database.ErrNotFound) {
					pending, pendingErr := w.store.HasPendingSessionQuiescence(ctx)
					if pendingErr != nil {
						err = pendingErr
					} else if !pending {
						delay = w.recoveryInterval
					}
				}
				if !errors.Is(err, database.ErrNotFound) && ctx.Err() == nil {
					logging.FromContext(ctx).ErrorContext(ctx, "claim runtime boundary", "error", err)
				}
				timer.Reset(delay)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				case <-w.quiescenceWake:
				}
			}
		})
	}
	workers.Wait()
	return nil
}

func (w *ActorWorkflow) quiesceIdleSession(ctx context.Context, work *database.SessionQuiescence) {
	runtimeCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	snapshot, err := w.Quiesce(runtimeCtx, work.Session)
	cancel()
	if err != nil {
		// No timeout-based takeover: the Substrate request may still complete.
		// Keep admission closed until its outcome can be safely reconciled.
		logging.FromContext(ctx).ErrorContext(ctx, "runtime boundary outcome unknown", "session_id", work.Session.Id, "version", work.Version, "error", err)
		return
	}
	// Keep a known snapshot until its reference is stored. Retry database failures
	// without repeating runtime work; give shutdown one bounded completion attempt.
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = w.store.FinishSessionQuiescence(finishCtx, work, snapshot)
		finishCancel()
		if err == nil {
			return
		}
		logging.FromContext(ctx).ErrorContext(ctx, "record idle runtime outcome", "session_id", work.Session.Id, "version", work.Version, "error", err)
		if errors.Is(err, database.ErrNotFound) || errors.Is(err, database.ErrConflict) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// quiesceInactiveSession suspends a runtime whose reported activity ended. The
// Session stays READY. An Actor that is no longer running only settles the
// boundary. No task snapshot is recorded: no task ended here, and the latest
// task keeps the snapshot of its own boundary.
func (w *ActorWorkflow) quiesceInactiveSession(ctx context.Context, work *database.SessionActivityQuiescence) {
	runtimeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := w.suspendInactiveRuntime(runtimeCtx, work.Session)
	cancel()
	if errors.Is(err, errActorResuming) {
		// Nothing was issued, so the claim can be released safely.
		w.deferInactiveSession(ctx, work)
		return
	}
	if err != nil {
		// No timeout-based takeover: the Substrate request may still complete.
		// The claim keeps admission closed until recovery or deletion retires it.
		logging.FromContext(ctx).ErrorContext(ctx, "runtime activity boundary outcome unknown", "session_id", work.Session.Id, "error", err)
		return
	}
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = w.store.FinishSessionActivityQuiescence(finishCtx, work)
		finishCancel()
		if err == nil || errors.Is(err, database.ErrNotFound) {
			return
		}
		logging.FromContext(ctx).ErrorContext(ctx, "record runtime activity outcome", "session_id", work.Session.Id, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// resumingRetry is how soon an activity boundary is claimed again after it
// found its Actor still resuming. A resume normally takes well under a second.
const resumingRetry = time.Second

// errActorResuming reports an Actor that is not running yet. No runtime
// call was issued, so its boundary is retried rather than settled.
var errActorResuming = errors.New("runtime Actor is still resuming")

func (w *ActorWorkflow) deferInactiveSession(ctx context.Context, work *database.SessionActivityQuiescence) {
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := w.store.DeferSessionActivityQuiescence(deferCtx, work, resumingRetry)
		cancel()
		if err == nil || errors.Is(err, database.ErrNotFound) {
			return
		}
		logging.FromContext(ctx).ErrorContext(ctx, "defer runtime activity boundary", "session_id", work.Session.Id, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *ActorWorkflow) suspendInactiveRuntime(ctx context.Context, session *apiv1.Session) error {
	revision, err := w.store.GetRuntimeRevision(ctx, session.GetPreparedRevision())
	if err != nil {
		return fmt.Errorf("load prepared revision: %w", err)
	}
	actor, err := w.actors.GetActor(ctx, revision.ActorTemplateAtespace, substrate.ActorName(session.GetId()))
	if err != nil {
		return fmt.Errorf("observe runtime Actor: %w", err)
	}
	if err := w.verifyActor(ctx, session, revision, actor); err != nil {
		return err
	}
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		return errActorResuming
	default:
		return nil
	}
	_, err = w.Quiesce(ctx, session)
	return err
}
