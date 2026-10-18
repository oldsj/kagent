package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

var suspendOperation = apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND

func requireSuspendBlocked(t *testing.T, client *Client, sessionID string) {
	t.Helper()
	_, err := client.BeginSessionOperation(t.Context(), sessionID, suspendOperation)
	require.ErrorIs(t, err, ErrFailedPrecondition, "Suspend must stay strict while work is unsettled")
}

func TestAbandonedBoundaryIsSettledForDeadRuntime(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateInputRequired} {
		t.Run(string(state), func(t *testing.T) {
			client := NewClient(setupTestDB(t))
			session, task, _ := quiescenceBoundaryFixture(t, client, state)
			// The runtime saved the boundary and died before reporting native cleanup.
			requireSuspendBlocked(t, client, session.Id)

			settled, err := client.SettleAbandonedSessionBoundaries(t.Context(), time.Hour, 10)
			require.NoError(t, err)
			require.Zero(t, settled, "a recent boundary still belongs to a live runtime")
			requireSuspendBlocked(t, client, session.Id)

			settled, err = client.SettleAbandonedSessionBoundaries(t.Context(), 0, 10)
			require.NoError(t, err)
			require.Equal(t, 1, settled)
			visible, err := client.GetSettledSessionTask(t.Context(), session.Id, string(task.ID), nil)
			require.NoError(t, err)
			require.Equal(t, state, visible.Status.State)
			settled, err = client.SettleAbandonedSessionBoundaries(t.Context(), 0, 10)
			require.NoError(t, err)
			require.Zero(t, settled, "settlement is idempotent")

			// The settled boundary now follows the ordinary idle path to Suspend.
			var snapshot *SessionTaskSnapshot
			if state.Terminal() {
				snapshot = &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}
			}
			work, err := client.ClaimSessionQuiescence(t.Context(), 0)
			require.NoError(t, err)
			require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, snapshot))
			if state.Terminal() {
				_, err = finishSessionOperation(t.Context(), client, session.Id, suspendOperation, "")
				require.NoError(t, err)
			}
		})
	}
}

func TestAbandonedBoundaryReaperSkipsDeletedSessions(t *testing.T) {
	client := NewClient(setupTestDB(t))
	session, _, _ := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
	require.NoError(t, deleteSession(t.Context(), client, session.Id))
	settled, err := client.SettleAbandonedSessionBoundaries(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Zero(t, settled)
}

func TestOrphanedQuiescenceClaimIsReclaimedAfterItsLease(t *testing.T) {
	for _, test := range []struct {
		name string
		// orphan leaves the claim as a crashed or older process would.
		orphan func(t *testing.T, client *Client, version int64) *SessionQuiescence
	}{
		{"lease ends", func(t *testing.T, client *Client, _ int64) *SessionQuiescence {
			work, err := client.ClaimSessionQuiescence(t.Context(), 20*time.Millisecond)
			require.NoError(t, err)
			_, err = client.ClaimSessionQuiescence(t.Context(), time.Hour)
			require.ErrorIs(t, err, ErrNotFound, "a live claim is never reassigned")
			time.Sleep(50 * time.Millisecond)
			return work
		}},
		{"claim without a deadline", func(t *testing.T, client *Client, version int64) *SessionQuiescence {
			// Claims written before leases existed carry an executor and no deadline.
			require.NoError(t, execSQL(t.Context(), client.db, `
				UPDATE session_task_event SET quiescence_executor_id = $2, quiescence_due_at = NULL WHERE sequence = $1
			`, version, uuid.New()))
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient(setupTestDB(t))
			session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
			stale := test.orphan(t, client, version)
			requireSuspendBlocked(t, client, session.Id) // Blocked until the next worker finishes the claim.

			work, err := client.ClaimSessionQuiescence(t.Context(), time.Hour)
			require.NoError(t, err, "an orphaned claim must be reclaimable")
			require.Equal(t, version, work.Version)
			if stale != nil {
				require.NotEqual(t, stale.ExecutorID, work.ExecutorID)
				require.ErrorIs(t, client.FinishSessionQuiescence(t.Context(), stale, &SessionTaskSnapshot{Atespace: "team-a", URI: "stale", ContentScope: "DATA"}), ErrNotFound,
					"the superseded executor cannot record an outcome")
			}
			_, err = client.ClaimSessionQuiescence(t.Context(), time.Hour)
			require.ErrorIs(t, err, ErrNotFound)
			require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}))
			_, err = finishSessionOperation(t.Context(), client, session.Id, suspendOperation, "")
			require.NoError(t, err)
		})
	}
}

func TestExpiredDispatchDoesNotBlockLifecycle(t *testing.T) {
	client, session := expirationFixture(t)
	require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "message"))
	requireSuspendBlocked(t, client, session.Id)
	require.NoError(t, execSQL(t.Context(), client.db, `
		UPDATE session SET dispatch_expires_at = clock_timestamp() - INTERVAL '1 second' WHERE id = $1
	`, session.Id))
	_, err := finishSessionOperation(t.Context(), client, session.Id, suspendOperation, "")
	require.NoError(t, err, "an abandoned dispatch lapses on its own")
}

// A session stuck behind each kind of unsettled work can still be deleted by the
// user, while Suspend and automatic idle deletion stay strict.
func TestUserDeleteDiscardsUnsettledWork(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage func(t *testing.T) (*Client, string)
	}{
		{"unpublished boundary", func(t *testing.T) (*Client, string) {
			client := NewClient(setupTestDB(t))
			session, _, _ := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
			return client, session.Id
		}},
		{"claimed quiescence", func(t *testing.T) (*Client, string) {
			client := NewClient(setupTestDB(t))
			session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
			_, err := client.ClaimSessionQuiescence(t.Context(), time.Hour)
			require.NoError(t, err)
			return client, session.Id
		}},
		{"live dispatch", func(t *testing.T) (*Client, string) {
			client, session := expirationFixture(t)
			require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "message"))
			return client, session.Id
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, id := test.stage(t)
			requireSuspendBlocked(t, client, id)
			_, err := client.BeginIdleSessionDeletion(t.Context(), id, time.Now(), time.Nanosecond)
			require.Error(t, err, "automatic idle deletion must not discard unsettled work")

			deleted, err := finishSessionOperation(t.Context(), client, id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE, "")
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, deleted.State)
			require.ErrorIs(t, client.ReserveSessionDispatch(t.Context(), id, uuid.New(), "next"), ErrConflict, "deleted sessions admit no work")
		})
	}
}
