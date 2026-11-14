package session

import (
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
