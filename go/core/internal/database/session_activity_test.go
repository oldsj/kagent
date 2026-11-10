package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func readySessionFixture(t *testing.T) (*Client, *apiv1alpha1.Session) {
	t.Helper()
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	session, err = markSessionReady(t.Context(), client, session.Id, "agent.example")
	require.NoError(t, err)
	return client, session
}

func activityPending(t *testing.T, client *Client, sessionID string) bool {
	t.Helper()
	pending, err := queryOne(t.Context(), client.db, `SELECT pending FROM session_activity WHERE session_id = $1`, pgx.RowTo[bool], sessionID)
	require.NoError(t, err)
	return pending
}

func TestRuntimeActivityAdoptsWakeOfSuspendedSession(t *testing.T) {
	client, operation, generation := observedSuspensionFixture(t)
	id := operation.Instance.Id

	// A report while Suspend is pending changes no lifecycle state.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), id, false, RuntimeActivityLease, 0))
	pending, err := client.GetSessionByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, pending.State)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, pending.Operation)

	suspended, err := client.FinishObservedSessionSuspension(t.Context(), id, operation.ID, operation.Instance.PreparedRevision, generation.ActorUID)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, suspended.State)
	require.False(t, activityPending(t, client, id), "a completed suspension satisfies the activity boundary")
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound)

	// Substrate ingress woke the Actor; its report adopts the wake.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), id, true, RuntimeActivityLease, 0))
	woken, err := client.GetSessionByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, woken.State)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, woken.Operation)
	require.Equal(t, suspended.A2AAuthority, woken.A2AAuthority)
	require.Equal(t, suspended.Name, woken.Name)
	require.True(t, activityPending(t, client, id))

	// The adopted Session accepts new work like any READY Session.
	require.NoError(t, client.ReserveSessionDispatch(t.Context(), id, uuid.New(), "after wake"))
}

func TestRuntimeActivityRefusesDeletingSession(t *testing.T) {
	client, session := readySessionFixture(t)
	_, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.NoError(t, err)
	require.ErrorIs(t, client.RecordRuntimeActivity(t.Context(), session.Id, true, RuntimeActivityLease, 0), ErrFailedPrecondition)
}

func TestRuntimeActivityBoundaryClaim(t *testing.T) {
	client, session := readySessionFixture(t)
	_, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "a runtime that never reported has no boundary")

	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, true, RuntimeActivityLease, 0))
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "an active lease holds idle quiescence")

	// An idle report is due after the quiescence delay.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, time.Hour))
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	work, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, session.Id, work.Session.Id)
	require.False(t, work.Waiting)
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "one claim at a time")

	// A held claim fences new execution like a task-boundary claim.
	task := newSessionTask(uuid.NewString(), "during claim")
	task.ContextID = session.ContextId
	_, err = client.CreateRuntimeTask(t.Context(), session.Id, taskMutationHash("during claim"), task, "")
	require.ErrorIs(t, err, ErrFailedPrecondition)
	require.ErrorIs(t, client.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "during claim"), ErrDispatchBusy)

	// A report after the claim keeps the boundary pending for the next claim.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	require.NoError(t, client.FinishSessionActivityQuiescence(t.Context(), work))
	require.ErrorIs(t, client.FinishSessionActivityQuiescence(t.Context(), work), ErrNotFound, "a finished claim cannot finish again")
	require.True(t, activityPending(t, client, session.Id))

	again, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.NotEqual(t, work.ExecutorID, again.ExecutorID)
	require.NoError(t, client.FinishSessionActivityQuiescence(t.Context(), again))
	require.False(t, activityPending(t, client, session.Id))
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "after claim"))
}

func TestRuntimeActivityClaimTakeoverAfterLease(t *testing.T) {
	client, session := readySessionFixture(t)
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	stale, err := client.ClaimSessionActivityQuiescence(t.Context(), time.Millisecond)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "an expired claim lease is taken over")
	require.ErrorIs(t, client.FinishSessionActivityQuiescence(t.Context(), stale), ErrNotFound, "the superseded executor cannot finish")
}

func TestRuntimeActivityWaitsForTaskWork(t *testing.T) {
	client := NewClient(setupTestDB(t))
	session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	_, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "unsettled native cleanup blocks activity quiescence")

	// A client attached while the turn settles holds the task boundary too.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, true, RuntimeActivityLease, 0))
	require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
	_, err = client.ClaimSessionQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "a live activity lease holds task-boundary quiescence")
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound)

	// The task boundary is claimed first and records its own snapshot.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	_, err = client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound, "a pending task boundary goes first")
	work, err := client.ClaimSessionQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}))
	activity, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.False(t, activity.Waiting)
}

func TestRuntimeActivityDelaysTaskQuiescenceByQuiesceDelay(t *testing.T) {
	client := NewClient(setupTestDB(t))
	session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
	require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
	// The client left; the boundary waits for the activity delay, not just the lease.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, time.Hour))
	_, err := client.ClaimSessionQuiescence(t.Context(), 0)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRuntimeActivityPausesWaitingTurn(t *testing.T) {
	client := NewClient(setupTestDB(t))
	session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateInputRequired)
	require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
	work, err := client.ClaimSessionQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, nil))
	// A client woke the paused runtime and then left.
	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, false, RuntimeActivityLease, 0))
	activity, err := client.ClaimSessionActivityQuiescence(t.Context(), 0)
	require.NoError(t, err)
	require.True(t, activity.Waiting, "a waiting turn keeps its native process: pause, do not suspend")
}

func TestRuntimeActivityExtendsIdleExpiration(t *testing.T) {
	client, session := readySessionFixture(t)
	now := databaseTime(t, client).Add(time.Hour)
	ttl := 30 * time.Minute

	require.NoError(t, client.RecordRuntimeActivity(t.Context(), session.Id, true, RuntimeActivityLease, 0))
	_, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, databaseTime(t, client).Add(time.Hour), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "a live activity lease is not idle")

	_, err = queryOne(t.Context(), client.db, `
		UPDATE session_activity SET reported_at = $2, active_until = clock_timestamp() WHERE session_id = $1 RETURNING session_id
	`, pgx.RowTo[uuid.UUID], session.Id, now.Add(-ttl/2))
	require.NoError(t, err)
	ids, err := client.ListIdleSessions(t.Context(), now, ttl, "", 10)
	require.NoError(t, err)
	require.NotContains(t, ids, session.Id, "a recent report keeps the Session alive")
	_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, now, ttl)
	require.ErrorIs(t, err, ErrConflict)

	later := now.Add(ttl)
	ids, err = client.ListIdleSessions(t.Context(), later, ttl, "", 10)
	require.NoError(t, err)
	require.Contains(t, ids, session.Id)
	_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, later, ttl)
	require.NoError(t, err)
}
