package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRuntimeActivityQuiescence(t *testing.T) {
	t.Run("Given an active report, when its lease is running, assert that no idle suspension is claimable", func(t *testing.T) {
		client, sessionID := givenReadySession(t)

		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Minute, 0))

		assertQuiescencePending(t, client, false)
		_, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Given an active runtime, when it reports idle, assert that its suspension is claimable and the boundary closes on finish", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Minute, 0))

		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, time.Minute, 0))

		assertQuiescencePending(t, client, true)
		work, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)
		require.Equal(t, sessionID, work.Session.Id)
		require.NoError(t, client.FinishSessionActivityQuiescence(t.Context(), work))
		assertQuiescencePending(t, client, false)
		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Given an idle report with a quiescence delay, when the delay has not passed, assert that suspension waits", func(t *testing.T) {
		client, sessionID := givenReadySession(t)

		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, time.Minute, 15*time.Minute))

		_, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound)
		givenActivityDue(t, client, sessionID)
		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)
	})

	t.Run("Given a claimed activity boundary, when the runtime reports again before finish, assert that the boundary stays pending", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		work, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)

		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		require.NoError(t, client.FinishSessionActivityQuiescence(t.Context(), work))

		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err, "a report after the claim proves the runtime ran again")
	})

	t.Run("Given a claimed activity boundary, when the worker defers it, assert that it is claimable again only after the retry", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		work, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)

		require.NoError(t, client.DeferSessionActivityQuiescence(t.Context(), work, time.Hour))

		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound, "a deferred boundary waits for its retry")
		require.ErrorIs(t, client.FinishSessionActivityQuiescence(t.Context(), work), ErrNotFound, "deferral released the claim")
		givenActivityDue(t, client, sessionID)
		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err, "the deferred boundary stays pending")
	})

	t.Run("Given a claimed activity boundary, when the runtime reports active before the worker defers it, assert that the lease still holds suspension", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		work, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Hour, 0))

		require.NoError(t, client.DeferSessionActivityQuiescence(t.Context(), work, 0))

		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound, "deferral never brings a later idle deadline forward")
	})

	t.Run("Given a claimed activity boundary, when a new turn is dispatched, assert that execution is fenced until finish", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		work, err := client.ClaimSessionActivityQuiescence(t.Context())
		require.NoError(t, err)

		session, err := client.GetSessionByID(t.Context(), sessionID)
		require.NoError(t, err)
		next := newSessionTask(uuid.NewString(), "next")
		next.ContextID, next.Status.State = session.ContextId, a2a.TaskStateWorking
		_, err = client.CreateRuntimeTask(t.Context(), sessionID, taskMutationHash("next"), next, "")
		require.ErrorIs(t, err, ErrFailedPrecondition)

		require.NoError(t, client.FinishSessionActivityQuiescence(t.Context(), work))
		_, err = client.CreateRuntimeTask(t.Context(), sessionID, taskMutationHash("next"), next, "")
		require.NoError(t, err)
	})

	t.Run("Given a settled turn and an attached client, when the turn's boundary is due, assert that the active lease holds task suspension", func(t *testing.T) {
		client, sessionID, taskID, version := givenSavedWaitingBoundary(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Minute, 0))

		require.NoError(t, client.SettleSessionTask(t.Context(), sessionID, taskID, version, 0))

		assertQuiescencePending(t, client, false)
		_, err := client.ClaimSessionQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound)
		_, err = client.ClaimSessionActivityQuiescence(t.Context())
		require.ErrorIs(t, err, ErrNotFound, "the task boundary's own claim records the snapshot")

		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, time.Minute, 0))
		work, err := client.ClaimSessionQuiescence(t.Context())
		require.NoError(t, err)
		require.Equal(t, version, work.Version)
	})

	t.Run("Given a deleting Session, when its runtime reports, assert that the report is rejected", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		_, err := client.BeginIdleSessionDeletion(t.Context(), sessionID, time.Now().Add(time.Hour), time.Hour)
		require.NoError(t, err)

		err = client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Minute, 0)

		require.ErrorIs(t, err, ErrFailedPrecondition)
	})
}

func TestRuntimeActivityExtendsIdleExpiration(t *testing.T) {
	t.Run("Given an idle Session, when its runtime reported recently, assert that expiration measures from the report", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, false, 0, 0))
		reportedAt := time.Now()

		_, err := client.BeginIdleSessionDeletion(t.Context(), sessionID, reportedAt.Add(-time.Second).Add(time.Hour), time.Hour)

		require.ErrorIs(t, err, ErrConflict)
	})

	t.Run("Given an active lease, when the idle TTL has elapsed, assert that expiration is refused", func(t *testing.T) {
		client, sessionID := givenReadySession(t)
		require.NoError(t, client.RecordRuntimeActivity(t.Context(), sessionID, true, time.Hour, 0))

		_, err := client.BeginIdleSessionDeletion(t.Context(), sessionID, time.Now().Add(time.Minute).Add(time.Hour), time.Hour)

		require.ErrorIs(t, err, ErrConflict)
	})
}

func givenReadySession(t *testing.T) (*Client, string) {
	t.Helper()
	client, session := expirationFixture(t)
	return client, session.Id
}

func givenActivityDue(t *testing.T, client *Client, sessionID string) {
	t.Helper()
	require.NoError(t, execSQL(t.Context(), client.db, `
		UPDATE session_activity SET idle_due_at = clock_timestamp() - INTERVAL '1 second' WHERE session_id = $1
	`, sessionID))
}
