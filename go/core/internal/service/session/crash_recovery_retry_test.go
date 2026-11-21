package session

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

func TestBackgroundRetryOfAdmittedCrashRecovery(t *testing.T) {
	t.Run("Given a crashed Session whose idle revert failed, when Substrate can reach the Actor again and a retry pass runs, assert that the Session is reverted and accepts input", func(t *testing.T) {
		fixture := newCrashRetryFixture(t)
		fixture.substrateUnavailable = false

		fixture.WhenARetryPassRuns()

		fixture.AssertThatTheSessionIsReady()
		require.Equal(t, 2, fixture.reverts, "the idle worker's revert and the background retry")
	})

	t.Run("Given a crashed Session whose revert keeps failing, when passes run before and after its backoff, assert that it is retried only once due with a doubling delay", func(t *testing.T) {
		fixture := newCrashRetryFixture(t)

		fixture.WhenARetryPassRuns()
		require.Equal(t, 2, fixture.reverts)
		require.Equal(t, crashRecoveryRetryMin, fixture.retries.pending[fixture.session.Id].delay)

		fixture.clock = fixture.clock.Add(crashRecoveryRetryMin - time.Second)
		fixture.WhenARetryPassRuns()
		require.Equal(t, 2, fixture.reverts, "not due yet")

		fixture.clock = fixture.clock.Add(time.Second)
		fixture.WhenARetryPassRuns()
		require.Equal(t, 3, fixture.reverts)
		require.Equal(t, 2*crashRecoveryRetryMin, fixture.retries.pending[fixture.session.Id].delay)

		fixture.AssertThatTheSessionStillAwaitsRecovery()
	})

	t.Run("Given a crashed Session whose revert keeps failing, when many retries fail, assert that the delay stops at its maximum", func(t *testing.T) {
		retries := newCrashRecoveryRetries(time.Now)
		for range 20 {
			retries.failed("s")
		}
		require.Equal(t, crashRecoveryRetryMax, retries.pending["s"].delay)
	})

	t.Run("Given a crashed Session whose recorded Actor is gone, when retry passes run, assert that retrying stops and the Session waits for a delete", func(t *testing.T) {
		fixture := newCrashRetryFixture(t)
		delete(fixture.actors.actors, actorKey("team-a", substrate.ActorName(fixture.session.Id)))

		fixture.WhenARetryPassRuns()
		fixture.clock = fixture.clock.Add(crashRecoveryRetryMax)
		fixture.WhenARetryPassRuns()

		require.True(t, fixture.retries.pending[fixture.session.Id].unrecoverable)
		require.Equal(t, 1, fixture.reverts, "only the idle worker's revert reached Substrate")
		fixture.AssertThatTheSessionStillAwaitsRecovery()
	})

	t.Run("Given a crashed Session whose recovery a live attempt holds, when a retry pass runs, assert that the pass leaves it to that attempt", func(t *testing.T) {
		fixture := newCrashRetryFixture(t)
		fixture.substrateUnavailable = false
		generation, err := fixture.store.GetSessionGeneration(t.Context(), fixture.session.Id)
		require.NoError(t, err)
		held, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		claimed, err := fixture.store.ClaimSessionOperation(held, fixture.session.Id, generation.OperationID, uuid.New())
		require.NoError(t, err)
		require.True(t, claimed)

		fixture.WhenARetryPassRuns()

		require.Equal(t, 1, fixture.reverts)
		require.Empty(t, fixture.retries.pending)
		fixture.AssertThatTheSessionStillAwaitsRecovery()
	})

	t.Run("Given a crashed Session recovered by a client, when a retry pass runs, assert that nothing is retried and its backoff is forgotten", func(t *testing.T) {
		fixture := newCrashRetryFixture(t)
		fixture.WhenARetryPassRuns()
		fixture.substrateUnavailable = false
		crashed, err := fixture.store.GetSessionByID(t.Context(), fixture.session.Id)
		require.NoError(t, err)
		_, err = fixture.workflow.Revert(t.Context(), crashed)
		require.NoError(t, err)
		reverts := fixture.reverts

		fixture.clock = fixture.clock.Add(crashRecoveryRetryMax)
		fixture.WhenARetryPassRuns()

		require.Equal(t, reverts, fixture.reverts)
		require.Empty(t, fixture.retries.pending)
		fixture.AssertThatTheSessionIsReady()
	})
}

type crashRetryFixture struct {
	t                    *testing.T
	store                *lifecycleTestStore
	actors               *lifecycleTestActors
	session              *apiv1.Session
	workflow             *ActorWorkflow
	retries              *crashRecoveryRetries
	clock                time.Time
	reverts              int
	substrateUnavailable bool
}

// newCrashRetryFixture leaves a Session as the idle worker does when its
// Actor crashed and the one revert it issued failed: FAILED, with an admitted
// REVERT and no live attempt.
func newCrashRetryFixture(t *testing.T) *crashRetryFixture {
	t.Helper()
	store, base, session, work := crashedIdleBoundaryFixture(t)
	f := &crashRetryFixture{t: t, store: store, actors: base, session: session, clock: time.Unix(1_800_000_000, 0), substrateUnavailable: true}
	revert := func(ctx context.Context, atespace, name string) (*ateapipb.Actor, error) {
		f.reverts++
		if f.substrateUnavailable {
			return nil, status.Error(codes.Unavailable, "while calling atelet Terminate: dial tcp 10.0.0.1:21998: i/o timeout")
		}
		return base.RevertActor(ctx, atespace, name)
	}
	refusal := status.Error(codes.FailedPrecondition, "MarkSuspending prerequisite not met (got: ACTOR_STATE_CRASHED, want ACTOR_STATE_RUNNING)")
	actors := &suspendRefusingActors{recoveryTestActors: &recoveryTestActors{lifecycleTestActors: base, revert: revert}, refusal: refusal}
	f.workflow = NewActorWorkflow(store, actors, make(chan struct{}, 1), time.Minute)
	f.retries = newCrashRecoveryRetries(func() time.Time { return f.clock })
	f.workflow.quiesceIdleSession(t.Context(), t.Context(), work)
	f.AssertThatTheSessionStillAwaitsRecovery()
	require.Equal(t, 1, f.reverts)
	return f
}

func (f *crashRetryFixture) WhenARetryPassRuns() {
	f.workflow.retryCrashRecoveries(f.t.Context(), f.retries)
}

func (f *crashRetryFixture) AssertThatTheSessionIsReady() {
	f.t.Helper()
	recovered, err := f.store.GetSessionByID(f.t.Context(), f.session.Id)
	require.NoError(f.t, err)
	require.Equal(f.t, apiv1.RuntimeState_RUNTIME_STATE_READY, recovered.State)
	require.Equal(f.t, apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE, recovered.Operation)
	require.NoError(f.t, f.store.ReserveSessionDispatch(f.t.Context(), f.session.Id, uuid.New(), uuid.NewString()))
}

func (f *crashRetryFixture) AssertThatTheSessionStillAwaitsRecovery() {
	f.t.Helper()
	crashed, err := f.store.GetSessionByID(f.t.Context(), f.session.Id)
	require.NoError(f.t, err)
	require.Equal(f.t, apiv1.RuntimeState_RUNTIME_STATE_FAILED, crashed.State)
	require.True(f.t, database.CrashRecoveryAdmitted(crashed))
	require.ErrorIs(f.t, f.store.ReserveSessionDispatch(f.t.Context(), f.session.Id, uuid.New(), uuid.NewString()), database.ErrConflict)
}
