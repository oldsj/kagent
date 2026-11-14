package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

func TestActivityBoundaryOfResumingActor(t *testing.T) {
	t.Run("Given an idle report sent while the woken Actor is still resuming, when the worker claims the boundary, assert that the Actor is suspended once it runs", func(t *testing.T) {
		store, session := lifecycleFixture(t)
		actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
		workflow := NewActorWorkflow(store, actors, make(chan struct{}, 1), time.Second)
		session, err := workflow.Create(t.Context(), session)
		require.NoError(t, err)
		actor := actors.actors[actorKey("team-a", substrate.ActorName(session.Id))]
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
		require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, false, 0, 0))
		work, err := store.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)

		workflow.quiesceInactiveSession(t.Context(), work)

		require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RESUMING, actor.Status.State, "a resuming Actor is not suspended")
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		var retried *database.SessionActivityQuiescence
		require.Eventually(t, func() bool {
			retried, err = store.ClaimSessionActivityQuiescence(t.Context())
			return err == nil
		}, 5*time.Second, 50*time.Millisecond, "the boundary stays pending until the Actor runs")
		workflow.quiesceInactiveSession(t.Context(), retried)
		require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, actor.Status.State)
	})
}

func TestDeferredActivityBoundaryWakesWorkers(t *testing.T) {
	t.Run("Given workers that recover only hourly, when an idle report finds the woken Actor still resuming, assert that the Actor is suspended once the deferral comes due", func(t *testing.T) {
		store, session := lifecycleFixture(t)
		base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
		session, err := NewActorWorkflow(store, base, make(chan struct{}, 1), time.Hour).Create(t.Context(), session)
		require.NoError(t, err)
		key := actorKey("team-a", substrate.ActorName(session.Id))
		base.mu.Lock()
		base.actors[key].Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		base.mu.Unlock()
		actors := &resumingOnceActors{lifecycleTestActors: base}
		wake := make(chan struct{}, 1)
		startQuiescenceWorker(t, NewActorWorkflow(store, actors, wake, time.Hour))

		require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, false, 0, 0))
		wake <- struct{}{}

		require.Eventually(t, func() bool {
			base.mu.Lock()
			defer base.mu.Unlock()
			return base.actors[key].Status.State == ateapipb.ActorState_ACTOR_STATE_SUSPENDED
		}, 5*time.Second, 20*time.Millisecond, "the deferred boundary is retried without waiting for recovery")
		require.True(t, actors.resumed.Load(), "the first claim saw the Actor resuming")
	})
}

// resumingOnceActors reports the Actor as resuming the first time it is read,
// as an idle report that arrives before the resume finishes sees it.
type resumingOnceActors struct {
	*lifecycleTestActors
	resumed atomic.Bool
}

func (a *resumingOnceActors) GetActor(ctx context.Context, atespace, name string) (*ateapipb.Actor, error) {
	actor, err := a.lifecycleTestActors.GetActor(ctx, atespace, name)
	if err == nil && !a.resumed.Swap(true) {
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
	}
	return actor, err
}
