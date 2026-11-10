package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/kagent-dev/kagent/go/pkg/logging"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var _ manager.Runnable = (*ActorWorkflow)(nil)
var _ manager.LeaderElectionRunnable = (*ActorWorkflow)(nil)

// Every API replica can process idle work; PostgreSQL grants each claim once.
func (*ActorWorkflow) NeedLeaderElection() bool { return false }

// Start pauses or suspends idle sessions independently of task publication:
// after a settled task boundary, and after reported runtime activity ends.
// A periodic scan discovers settled work across API replicas and restarts.
// Workers are bounded; task reads never wait for them.
func (w *ActorWorkflow) Start(ctx context.Context) error {
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			timer := time.NewTicker(time.Second)
			defer timer.Stop()
			for ctx.Err() == nil {
				work, err := w.store.ClaimSessionQuiescence(ctx, w.quiescenceLease())
				if err == nil {
					w.quiesceIdleSession(ctx, work)
					continue
				}
				if !errors.Is(err, database.ErrNotFound) && ctx.Err() == nil {
					logging.FromContext(ctx).ErrorContext(ctx, "claim runtime boundary", "error", err)
				}
				activity, err := w.store.ClaimSessionActivityQuiescence(ctx, w.quiescenceLease())
				if err == nil {
					w.quiesceInactiveSession(ctx, activity)
					continue
				}
				if !errors.Is(err, database.ErrNotFound) && ctx.Err() == nil {
					logging.FromContext(ctx).ErrorContext(ctx, "claim runtime activity boundary", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
		})
	}
	workers.Go(func() { w.settleAbandonedBoundaries(ctx) })
	workers.Wait()
	return nil
}

// quiescenceLease is how long a claim holds its boundary before another worker
// may take it over. It covers one runtime attempt plus the bounded retries that
// record its outcome.
func (w *ActorWorkflow) quiescenceLease() time.Duration {
	return w.attemptTimeout + time.Minute
}

// settleAbandonedBoundaries publishes boundaries whose runtime never reported
// native cleanup, so admission cannot stay closed behind a dead runtime. Every
// API replica may run it; settlement rechecks under the session lock.
func (w *ActorWorkflow) settleAbandonedBoundaries(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		settled, err := w.store.SettleAbandonedSessionBoundaries(ctx, database.AbandonedBoundaryAfter, 100)
		if err != nil && ctx.Err() == nil {
			logging.FromContext(ctx).ErrorContext(ctx, "settle abandoned runtime boundaries", "error", err)
		} else if settled > 0 {
			logging.FromContext(ctx).InfoContext(ctx, "settled abandoned runtime boundaries", "count", settled)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (w *ActorWorkflow) quiesceIdleSession(ctx context.Context, work *database.SessionQuiescence) {
	runtimeCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout)
	var snapshot *database.SessionTaskSnapshot
	var err error
	if work.State.Terminal() {
		snapshot, err = w.Quiesce(runtimeCtx, work.Session)
	} else {
		err = w.Pause(runtimeCtx, work.Session)
	}
	cancel()
	crashed := errors.Is(err, errActorCrashed)
	if err != nil && !crashed {
		// The Substrate request may still complete, so admission stays closed. The
		// claim lease expires and the next worker re-observes the Actor before acting.
		logging.FromContext(ctx).ErrorContext(ctx, "runtime boundary outcome unknown", "session_id", work.Session.Id, "version", work.Version, "error", err)
		return
	}
	if crashed {
		// The stop is final and took no checkpoint; settle it without one.
		logging.FromContext(ctx).WarnContext(ctx, "runtime crashed before its idle boundary; settling without a snapshot", "session_id", work.Session.Id, "version", work.Version)
	}
	// Keep a known snapshot until its reference is stored. Retry database failures
	// without repeating runtime work; give shutdown one bounded completion attempt.
	for delay := 100 * time.Millisecond; ; delay = min(2*delay, 5*time.Second) {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if crashed {
			err = w.store.FinishLostSessionQuiescence(finishCtx, work)
		} else {
			err = w.store.FinishSessionQuiescence(finishCtx, work, snapshot)
		}
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

// quiesceInactiveSession suspends a runtime whose reported activity ended, or
// pauses it while a turn waits for input. The Session stays READY. An Actor
// that is no longer running (idle work or an explicit lifecycle call already
// stopped it) only settles the boundary. No task snapshot is recorded: no task
// ended here, and the latest task keeps the snapshot of its own boundary.
func (w *ActorWorkflow) quiesceInactiveSession(ctx context.Context, work *database.SessionActivityQuiescence) {
	runtimeCtx, cancel := context.WithTimeout(ctx, w.attemptTimeout)
	err := w.quiesceInactiveRuntime(runtimeCtx, work)
	cancel()
	if err != nil && !errors.Is(err, errActorCrashed) {
		// The Substrate request may still complete, so admission stays closed until
		// the claim lease ends and the next worker re-observes the Actor.
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

func (w *ActorWorkflow) quiesceInactiveRuntime(ctx context.Context, work *database.SessionActivityQuiescence) error {
	generation, err := w.store.GetRuntimeGeneration(ctx, work.Session.GetId())
	if err != nil || generation.Phase != "active" {
		return fmt.Errorf("runtime generation unavailable")
	}
	actor, err := w.actors.GetActor(ctx, generation.Atespace, generation.ActorName)
	if err != nil {
		return fmt.Errorf("observe runtime Actor: %w", err)
	}
	if actor.GetMetadata().GetUid() != generation.ActorUID {
		return fmt.Errorf("runtime Actor %s/%s was replaced", generation.Atespace, generation.ActorName)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return nil
	}
	if work.Waiting {
		return w.Pause(ctx, work.Session)
	}
	_, err = w.Quiesce(ctx, work.Session)
	return err
}
