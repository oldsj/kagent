package session

import (
	"context"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

const activityTestOrigin = "http://kagent-controller.kagent:8083"

func activityActorState(t *testing.T, actors actorClient, store *lifecycleTestStore, id string) ateapipb.ActorState {
	t.Helper()
	actor, err := actors.GetActor(t.Context(), "team-a", fixtureActorName(t, store, id))
	require.NoError(t, err)
	return actor.GetStatus().GetState()
}

func startActivityWorker(t *testing.T, store workflowStore, actors actorClient) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- NewActorWorkflow(store, actors, fixtureCredentials{}, activityTestOrigin).Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
}

func TestIdleWorkerSuspendsRuntimeAfterReportedActivityEnds(t *testing.T) {
	store, session := lifecycleFixture(t)
	base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	actors := &retryTestActors{lifecycleTestActors: base}
	session, err := NewActorWorkflow(store, actors, fixtureCredentials{}, activityTestOrigin).Create(t.Context(), session)
	require.NoError(t, err)
	// Substrate ingress woke the Actor without the control plane.
	_, err = base.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
	require.NoError(t, err)
	require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, true, time.Minute, 0))
	before := actors.mutations.Load()
	startActivityWorker(t, store, actors)

	// A live lease keeps the runtime running.
	time.Sleep(1500 * time.Millisecond)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, activityActorState(t, actors, store, session.Id))
	require.Equal(t, before, actors.mutations.Load())

	// The last client left: kagent suspends it and the Session stays READY.
	require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, false, time.Minute, 0))
	require.Eventually(t, func() bool {
		return activityActorState(t, actors, store, session.Id) == ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return !activityPending(t, store, session.Id) }, 5*time.Second, 10*time.Millisecond)
	current, err := store.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, current.State)
	require.Equal(t, before+1, actors.mutations.Load(), "exactly one suspend")
}

func TestIdleWorkerOwnsRouterWakeOfExplicitSuspension(t *testing.T) {
	store, session := lifecycleFixture(t)
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, activityTestOrigin)
	session, err := workflow.Create(t.Context(), session)
	require.NoError(t, err)
	_, err = actors.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
	require.NoError(t, err)
	suspended, err := workflow.Suspend(t.Context(), session)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, suspended.State)

	// A client CONNECT through the router wakes the Actor; the runtime reports.
	_, err = actors.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
	require.NoError(t, err)
	require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, true, time.Minute, 0))
	woken, err := store.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, woken.State, "kagent no longer reports SUSPENDED while the runtime runs")
	association, err := workflow.InspectRuntime(t.Context(), woken)
	require.NoError(t, err)
	require.True(t, association.GetCurrentActive())

	startActivityWorker(t, store, actors)
	require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, false, time.Minute, 0))
	require.Eventually(t, func() bool {
		return activityActorState(t, actors, store, session.Id) == ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	}, 5*time.Second, 10*time.Millisecond)
	current, err := store.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, current.State)

	// The next wake is an ordinary READY activation, through ResumeSession or ingress.
	resumed, err := workflow.Resume(t.Context(), current)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.State)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, activityActorState(t, actors, store, session.Id))
}

func TestIdleWorkerSettlesActivityOfStoppedRuntime(t *testing.T) {
	store, session := lifecycleFixture(t)
	base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	actors := &retryTestActors{lifecycleTestActors: base}
	session, err := NewActorWorkflow(store, actors, fixtureCredentials{}, activityTestOrigin).Create(t.Context(), session)
	require.NoError(t, err)
	// Reported, then stopped by something else before the boundary was due.
	require.NoError(t, store.RecordRuntimeActivity(t.Context(), session.Id, false, time.Minute, 0))
	before := actors.mutations.Load()
	startActivityWorker(t, store, actors)
	require.Eventually(t, func() bool { return !activityPending(t, store, session.Id) }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, before, actors.mutations.Load(), "a suspended Actor needs no runtime call")
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, activityActorState(t, actors, store, session.Id))
}

// activityPending reads the boundary without claiming it, so a test never
// competes with the worker it observes.
func activityPending(t *testing.T, store *lifecycleTestStore, id string) bool {
	t.Helper()
	var pending bool
	require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT pending FROM session_activity WHERE session_id = $1`, id).Scan(&pending))
	return pending
}
