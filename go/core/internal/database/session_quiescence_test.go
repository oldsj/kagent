package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestBoundedQuiesceDelay(t *testing.T) {
	scenarios := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "negative clamps to zero", in: -time.Minute},
		{name: "zero stays zero"},
		{name: "within bound is kept", in: 15 * time.Minute, want: 15 * time.Minute},
		{name: "at bound is kept", in: 30 * time.Minute, want: 30 * time.Minute},
		{name: "above bound clamps", in: time.Hour, want: 30 * time.Minute},
		{name: "maximum duration clamps", in: time.Duration(1<<63 - 1), want: 30 * time.Minute},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			require.Equal(t, scenario.want, boundedQuiesceDelay(scenario.in))
		})
	}
}

func TestSessionQuiescenceDelay(t *testing.T) {
	t.Run("Given no delay, when a waiting turn settles, assert that its idle suspension is claimable", func(t *testing.T) {
		client, sessionID, taskID, version := givenSavedWaitingBoundary(t)

		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, 0))

		assertQuiescencePending(t, client, true)
		work, err := client.ClaimSessionQuiescence(t.Context())
		require.NoError(t, err)
		require.Equal(t, version, work.Version)
	})

	t.Run("Given a delay, when a waiting turn settles, assert that suspension waits for the deadline", func(t *testing.T) {
		client, sessionID, taskID, version := givenSavedWaitingBoundary(t)

		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, 15*time.Minute))

		visible, err := client.GetSettledSessionTask(t.Context(), sessionID, taskID, nil)
		require.NoError(t, err)
		require.Equal(t, a2a.TaskStateInputRequired, visible.Status.State, "publication is not delayed")
		assertQuiescencePending(t, client, false)
		_, err = client.ClaimSessionQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound)

		givenQuiescenceDeadlinePassed(t, client, version)
		assertQuiescencePending(t, client, true)
		work, err := client.ClaimSessionQuiescence(t.Context())
		require.NoError(t, err)
		require.Equal(t, version, work.Version)
	})

	t.Run("Given a settled boundary with a deadline, when settlement is retried, assert that the deadline is kept", func(t *testing.T) {
		client, sessionID, taskID, version := givenSavedWaitingBoundary(t)
		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, time.Minute))
		first := quiescenceDeadline(t, client, version)

		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, 30*time.Minute))

		require.Equal(t, first, quiescenceDeadline(t, client, version))
	})

	t.Run("Given a boundary published by an older controller, when a worker claims, assert that a NULL deadline is eligible now", func(t *testing.T) {
		client, sessionID, taskID, version := givenSavedWaitingBoundary(t)
		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, 15*time.Minute))

		require.NoError(t, execSQL(t.Context(), client.db, `UPDATE session_task_event SET quiescence_due_at = NULL WHERE sequence = $1`, version))

		assertQuiescencePending(t, client, true)
		_, err := client.ClaimSessionQuiescence(t.Context())
		require.NoError(t, err)
	})
}

// givenSavedWaitingBoundary leaves an INPUT_REQUIRED boundary saved but not settled.
func givenSavedWaitingBoundary(t *testing.T) (*Client, string, string, int64) {
	t.Helper()
	client := NewClient(setupTestDB(t), "public")
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, task := waitingTaskFixture(t, client)
	_, version, err := client.GetVersionedSessionTask(t.Context(), session.Id, string(task.ID))
	require.NoError(t, err)
	for _, state := range []a2a.TaskState{a2a.TaskStateWorking, a2a.TaskStateInputRequired} {
		task.Status.State = state
		version, err = client.UpdateSessionTask(t.Context(), session.Id, version, taskMutationHash(string(state)), task, task, "")
		require.NoError(t, err)
	}
	return client, session.Id, string(task.ID), version
}

func givenQuiescenceDeadlinePassed(t *testing.T, client *Client, version int64) {
	t.Helper()
	require.NoError(t, execSQL(t.Context(), client.db, `
		UPDATE session_task_event SET quiescence_due_at = clock_timestamp() - INTERVAL '1 second' WHERE sequence = $1
	`, version))
}

func quiescenceDeadline(t *testing.T, client *Client, version int64) time.Time {
	t.Helper()
	due, err := queryOne(t.Context(), client.db, `SELECT quiescence_due_at FROM session_task_event WHERE sequence = $1`, pgx.RowTo[*time.Time], version)
	require.NoError(t, err)
	require.NotNil(t, due)
	return *due
}

func assertQuiescencePending(t *testing.T, client *Client, want bool) {
	t.Helper()
	pending, err := client.HasPendingSessionQuiescence(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, pending)
}
