package session

import (
	"crypto/sha256"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// quiescedFixture settles one completed turn and lets the idle worker suspend
// its Actor, leaving the Session logically READY with no running association.
func quiescedFixture(t *testing.T) (*lifecycleTestStore, *retryTestActors, *apiv1alpha1.Session, *ateapipb.Actor) {
	t.Helper()
	store, session := lifecycleFixture(t)
	base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	session, err := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
	require.NoError(t, err)
	name := fixtureActorName(t, store, session.Id)
	_, err = base.ResumeActor(t.Context(), "team-a", name)
	require.NoError(t, err)
	settleTask(t, store, session, a2a.TaskStateCompleted)
	work, err := store.ClaimSessionQuiescence(t.Context())
	require.NoError(t, err)
	snapshot, err := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Quiesce(t.Context(), work.Session)
	require.NoError(t, err)
	require.NoError(t, store.FinishSessionQuiescence(t.Context(), work, snapshot))
	actor := base.actors[actorKey("team-a", name)]
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, actor.Status.State)
	current, err := store.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, current.State)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.Operation)
	association, err := NewActorWorkflow(store, base, fixtureCredentials{}, "").InspectRuntime(t.Context(), current)
	require.NoError(t, err)
	require.Nil(t, association, "a quiesced Actor has no running association")
	return store, &retryTestActors{lifecycleTestActors: base}, current, actor
}

func settleTask(t *testing.T, store *lifecycleTestStore, session *apiv1alpha1.Session, state a2a.TaskState) {
	t.Helper()
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	message.ContextID = session.ContextId
	task := a2a.NewSubmittedTask(message, message)
	task.Status.State = state
	hash := sha256.Sum256([]byte(uuid.NewString()))
	version, err := store.CreateRuntimeTask(t.Context(), session.Id, hash[:], task, "")
	require.NoError(t, err)
	if state.Terminal() || state == a2a.TaskStateInputRequired {
		require.NoError(t, store.SettleSessionTask(t.Context(), session.Id, string(task.ID), version))
	}
}

func TestResumeActivatesQuiescedReadySessionInPlace(t *testing.T) {
	for _, quiesced := range []ateapipb.ActorState{ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_PAUSED} {
		t.Run(quiesced.String(), func(t *testing.T) {
			store, actors, session, actor := quiescedFixture(t)
			actor.Status.State = quiesced
			before, err := store.GetRuntimeGeneration(t.Context(), session.Id)
			require.NoError(t, err)
			workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")

			resumed, err := workflow.Resume(t.Context(), session)
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.State)
			require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, resumed.Operation)
			require.Equal(t, session.ContextId, resumed.ContextId)
			require.Equal(t, session.A2AAuthority, resumed.A2AAuthority)
			require.EqualValues(t, 1, actors.mutations.Load(), "exactly one ResumeActor")
			require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, actor.Status.State)
			require.Equal(t, "actor-uid", actor.Metadata.Uid)
			require.Len(t, actors.actors, 1)

			after, err := store.GetRuntimeGeneration(t.Context(), session.Id)
			require.NoError(t, err)
			require.Equal(t, before.ID, after.ID)
			require.Equal(t, before.ActorUID, after.ActorUID)
			require.Equal(t, "active", after.Phase)
			association, err := workflow.InspectRuntime(t.Context(), resumed)
			require.NoError(t, err)
			require.NotNil(t, association)
			require.Equal(t, before.ID.String(), association.GenerationId)
			require.Equal(t, "actor-uid", association.ActorUid)

			again, err := workflow.Resume(t.Context(), resumed)
			require.NoError(t, err)
			require.Equal(t, resumed.State, again.State)
			require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, again.Operation)
			require.EqualValues(t, 1, actors.mutations.Load(), "a running Actor needs no second ResumeActor")
		})
	}
}

func TestResumeActivationRefusals(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *lifecycleTestStore, *apiv1alpha1.Session, *ateapipb.Actor)
		want    error
	}{
		{name: "changed Actor UID", want: database.ErrFailedPrecondition, prepare: func(_ *testing.T, _ *lifecycleTestStore, _ *apiv1alpha1.Session, actor *ateapipb.Actor) {
			actor.Metadata.Uid = "replacement-uid"
		}},
		{name: "revoked generation", want: database.ErrFailedPrecondition, prepare: func(t *testing.T, store *lifecycleTestStore, session *apiv1alpha1.Session, _ *ateapipb.Actor) {
			require.NoError(t, store.RevokeRuntimeGeneration(t.Context(), session.Id))
		}},
		{name: "busy dispatch", want: database.ErrFailedPrecondition, prepare: func(t *testing.T, store *lifecycleTestStore, session *apiv1alpha1.Session, _ *ateapipb.Actor) {
			require.NoError(t, store.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""))
		}},
		{name: "active turn", want: database.ErrConflict, prepare: func(t *testing.T, store *lifecycleTestStore, session *apiv1alpha1.Session, _ *ateapipb.Actor) {
			settleTask(t, store, session, a2a.TaskStateWorking)
		}},
		{name: "pending quiescence", want: database.ErrConflict, prepare: func(t *testing.T, store *lifecycleTestStore, session *apiv1alpha1.Session, _ *ateapipb.Actor) {
			settleTask(t, store, session, a2a.TaskStateCompleted)
		}},
		{name: "unsettled Actor", want: database.ErrConflict, prepare: func(_ *testing.T, _ *lifecycleTestStore, _ *apiv1alpha1.Session, actor *ateapipb.Actor) {
			actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDING
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, actors, session, actor := quiescedFixture(t)
			test.prepare(t, store, session, actor)
			_, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Resume(t.Context(), session)
			require.ErrorIs(t, err, test.want)
			require.Zero(t, actors.mutations.Load(), "no runtime mutation")
			require.NotEqual(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, actor.Status.State)
			current, err := store.GetSessionByID(t.Context(), session.Id)
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, current.State)
			require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.Operation, "refusal leaves no operation behind")
		})
	}
}

// A store-level race: the Actor UID or generation changes between the
// workflow's observation and admission.
func TestBeginSessionActivationFencesObservedIdentity(t *testing.T) {
	store, _, session, _ := quiescedFixture(t)
	generation, err := store.GetRuntimeGeneration(t.Context(), session.Id)
	require.NoError(t, err)
	_, err = store.BeginSessionActivation(t.Context(), session.Id, generation.ID, "replacement-uid")
	require.ErrorIs(t, err, database.ErrFailedPrecondition)
	_, err = store.BeginSessionActivation(t.Context(), session.Id, uuid.New(), generation.ActorUID)
	require.ErrorIs(t, err, database.ErrFailedPrecondition)

	first, err := store.BeginSessionActivation(t.Context(), session.Id, generation.ID, generation.ActorUID)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME, first.Instance.Operation)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, first.Instance.State)
	joined, err := store.BeginSessionActivation(t.Context(), session.Id, generation.ID, generation.ActorUID)
	require.NoError(t, err)
	require.Equal(t, first.ID, joined.ID, "a concurrent activation joins the pending operation")
}

func TestUncertainActivationHoldsAdmissionAndReconcilesSameActor(t *testing.T) {
	store, actors, session, actor := quiescedFixture(t)
	actors.mutationErr = status.Error(codes.Unavailable, "lost resume response")
	workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
	_, err := workflow.Resume(t.Context(), session)
	require.Error(t, err)
	require.EqualValues(t, 1, actors.mutations.Load())

	pending, err := store.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME, pending.Operation, "uncertain work stays pending")
	association, err := workflow.InspectRuntime(t.Context(), pending)
	require.NoError(t, err)
	require.Nil(t, association, "no attestation while the outcome is uncertain")
	require.ErrorIs(t, store.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""), database.ErrConflict, "no dispatch while uncertain")
	_, err = store.ClaimSessionQuiescence(t.Context())
	require.ErrorIs(t, err, database.ErrNotFound)
	_, err = workflow.Suspend(t.Context(), pending)
	require.ErrorIs(t, err, database.ErrConflict, "no second lifecycle writer")

	actors.mutationErr = nil
	resumed, err := workflow.Resume(t.Context(), pending)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, resumed.Operation)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.State)
	require.Equal(t, "actor-uid", actor.Metadata.Uid)
	require.Len(t, actors.actors, 1)
	association, err = workflow.InspectRuntime(t.Context(), resumed)
	require.NoError(t, err)
	require.NotNil(t, association)
	require.Equal(t, "actor-uid", association.ActorUid)
}
