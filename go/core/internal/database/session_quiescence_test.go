package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestBoundedQuiesceDelay(t *testing.T) {
	for _, test := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "negative", in: -time.Minute},
		{name: "zero"},
		{name: "within bound", in: 15 * time.Minute, want: 15 * time.Minute},
		{name: "at bound", in: 30 * time.Minute, want: 30 * time.Minute},
		{name: "above bound", in: time.Hour, want: 30 * time.Minute},
		{name: "maximum duration", in: time.Duration(1<<63 - 1), want: 30 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, boundedQuiesceDelay(test.in))
		})
	}
}

func TestLegacySettlementAgainstMigration7(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		for _, mode := range []string{"rolling upgrade", "binary rollback"} {
			t.Run(string(state)+"/"+mode, func(t *testing.T) {
				db := setupTestDB(t)
				client := NewClient(db)
				session, task, version := quiescenceBoundaryFixture(t, client, state)
				var snapshot *SessionTaskSnapshot
				if state.Terminal() {
					snapshot = &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}
				}
				if mode == "binary rollback" {
					// Follow the rollback order: the new binary settles at zero delay
					// before an old binary starts against the retained schema.
					require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
					work, err := client.ClaimSessionQuiescence(t.Context())
					require.NoError(t, err)
					require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, snapshot))
					pool, err := pgxpool.NewWithConfig(t.Context(), db.Config().Copy())
					require.NoError(t, err)
					t.Cleanup(pool.Close)
					client = NewClient(pool)
					session, task, version = quiescenceBoundaryFixture(t, client, state)
				}
				migrated, err := queryOne(t.Context(), client.db, `
					SELECT max(version_id) FROM schema_migrations WHERE is_applied
				`, pgx.RowTo[int64])
				require.NoError(t, err)
				require.EqualValues(t, 7, migrated, "binary rollback must keep schema 7")
				// This is the base's actual settlement implementation, not a SQL
				// approximation or the new writer configured with delay zero.
				for range 2 {
					require.NoError(t, client.legacySettleSessionTask(t.Context(), session.Id, string(task.ID), version))
				}
				visible, err := client.GetSettledSessionTask(t.Context(), session.Id, string(task.ID), nil)
				require.NoError(t, err)
				require.Equal(t, state, visible.Status.State)
				require.Len(t, visible.History, len(task.History))
				// A later new-binary acknowledgement cannot delay an old publication.
				require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 15*time.Minute))
				due, err := queryOne(t.Context(), client.db, `
					SELECT quiescence_due_at FROM session_task_event WHERE sequence = $1
				`, pgx.RowTo[*time.Time], version)
				require.NoError(t, err)
				require.Nil(t, due, "old writers leave the deadline unset")
				work, err := client.ClaimSessionQuiescence(t.Context())
				require.NoError(t, err, "NULL must mean eligible now in selection and claim")
				require.Equal(t, version, work.Version)
				_, err = client.ClaimSessionQuiescence(t.Context())
				require.ErrorIs(t, err, ErrNotFound)
				require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, snapshot))
				dispatchID := uuid.New()
				require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, dispatchID, "next"))
			})
		}
	}
}

// quiescenceBoundaryFixture stages a native boundary without acknowledging cleanup.
func quiescenceBoundaryFixture(t *testing.T, client *Client, state a2a.TaskState) (*apiv1alpha1.Session, *a2a.Task, int64) {
	t.Helper()
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, waiting := waitingTaskFixture(t, client)
	reply := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("continue"))
	reply.TaskID, reply.ContextID = waiting.ID, waiting.ContextID
	task, version := resumeRuntimeTask(t, client, session.Id, reply)
	task.Status = a2a.TaskStatus{State: state, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("boundary"))}
	version, err := client.UpdateSessionTask(t.Context(), session.Id, version, taskMutationHash("boundary"), task, task, "")
	require.NoError(t, err)
	return session, task, version
}

func quiescenceDeadline(t *testing.T, client *Client, version int64) time.Time {
	t.Helper()
	due, err := queryOne(t.Context(), client.db, `
		SELECT quiescence_due_at FROM session_task_event WHERE sequence = $1
	`, pgx.RowTo[time.Time], version)
	require.NoError(t, err)
	return due
}

func databaseTime(t *testing.T, client *Client) time.Time {
	t.Helper()
	now, err := queryOne(t.Context(), client.db, `SELECT clock_timestamp()`, pgx.RowTo[time.Time])
	require.NoError(t, err)
	return now
}

func TestSessionQuiescenceDelayEligibility(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateInputRequired} {
		for _, delay := range []time.Duration{0, time.Second} {
			t.Run(string(state)+"/"+delay.String(), func(t *testing.T) {
				client := NewClient(setupTestDB(t))
				session, task, version := quiescenceBoundaryFixture(t, client, state)
				_, err := client.ClaimSessionQuiescence(t.Context())
				require.ErrorIs(t, err, ErrNotFound, "native cleanup still gates eligibility")
				before := databaseTime(t, client)
				require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, delay))
				after := databaseTime(t, client)
				due := quiescenceDeadline(t, client, version)
				require.False(t, due.Before(before.Add(delay)))
				require.False(t, due.After(after.Add(delay)))
				visible, err := client.GetSettledSessionTask(t.Context(), session.Id, string(task.ID), nil)
				require.NoError(t, err)
				require.Equal(t, state, visible.Status.State, "publication is immediate")
				// Replayed acknowledgements cannot shorten or extend the deadline.
				require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 30*time.Minute))
				require.Equal(t, due, quiescenceDeadline(t, client, version))
				var work *SessionQuiescence
				if delay > 0 {
					_, err := client.ClaimSessionQuiescence(t.Context())
					require.ErrorIs(t, err, ErrNotFound)
					require.Eventually(t, func() bool {
						work, err = client.ClaimSessionQuiescence(t.Context())
						return err == nil
					}, 5*time.Second, 10*time.Millisecond)
				} else {
					work, err = client.ClaimSessionQuiescence(t.Context())
					require.NoError(t, err)
				}
				require.Equal(t, version, work.Version)
				_, err = client.ClaimSessionQuiescence(t.Context())
				require.ErrorIs(t, err, ErrNotFound, "only one worker can claim")
				var snapshot *SessionTaskSnapshot
				if state.Terminal() {
					snapshot = &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}
				}
				require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, snapshot))
			})
		}
	}
}

func TestNewTurnSupersedesDelayedQuiescence(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateInputRequired} {
		t.Run(string(state), func(t *testing.T) {
			client := NewClient(setupTestDB(t))
			session, task, version := quiescenceBoundaryFixture(t, client, state)
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, time.Second))
			due := quiescenceDeadline(t, client, version)
			dispatchID := uuid.New()
			require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, dispatchID, "next"))
			_, err := client.ClaimSessionQuiescence(t.Context())
			require.ErrorIs(t, err, ErrNotFound)
			next := newSessionTask(uuid.NewString(), "next")
			next.ContextID = session.ContextId
			var nextVersion int64
			if state.Terminal() {
				nextVersion, err = client.CreateRuntimeTask(t.Context(), session.Id, taskMutationHash("next"), next, dispatchID.String())
			} else {
				next.ID = task.ID
				next.History = task.History
				nextVersion, err = client.UpdateSessionTask(t.Context(), session.Id, version, taskMutationHash("next"), next, next, dispatchID.String())
			}
			require.NoError(t, err)
			require.True(t, databaseTime(t, client).Before(due), "new turn was accepted during the delay")
			require.Eventually(t, func() bool { return databaseTime(t, client).After(due) }, 5*time.Second, 10*time.Millisecond)
			// An old cleanup retry must not requeue the superseded boundary.
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
			_, err = client.ClaimSessionQuiescence(t.Context())
			require.ErrorIs(t, err, ErrNotFound, "a running turn cannot be paused")
			// The next boundary owns its own deadline and can still quiesce.
			next.Status.State = a2a.TaskStateCompleted
			nextVersion, err = client.UpdateSessionTask(t.Context(), session.Id, nextVersion, taskMutationHash("next boundary"), next, next, "")
			require.NoError(t, err)
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(next.ID), nextVersion, 0))
			work, err := client.ClaimSessionQuiescence(t.Context())
			require.NoError(t, err)
			require.Equal(t, nextVersion, work.Version)
		})
	}
}

func TestDelayedQuiescenceRemainsDispatchFenced(t *testing.T) {
	client := NewClient(setupTestDB(t))
	session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
	require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, time.Second))
	due := quiescenceDeadline(t, client, version)
	dispatchID := uuid.New()
	require.NoError(t, client.ReserveSessionDispatch(t.Context(), session.Id, dispatchID, ""))
	// The deadline may pass while the new turn is still crossing the network.
	require.Eventually(t, func() bool { return databaseTime(t, client).After(due) }, 5*time.Second, 10*time.Millisecond)
	_, err := client.ClaimSessionQuiescence(t.Context())
	require.ErrorIs(t, err, ErrNotFound)
	notAccepted, err := client.RevokeSessionDispatch(t.Context(), session.Id, dispatchID, "next")
	require.NoError(t, err)
	require.True(t, notAccepted)
	work, err := client.ClaimSessionQuiescence(t.Context())
	require.NoError(t, err)
	require.Equal(t, version, work.Version)
}

func TestSessionQuiescenceDelaySurvivesRestart(t *testing.T) {
	db := setupTestDB(t)
	client := NewClient(db)
	session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
	require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, time.Second))
	due := quiescenceDeadline(t, client, version)
	// A fresh pool models a new controller process; it shares no scheduling state.
	pool, err := pgxpool.NewWithConfig(t.Context(), db.Config().Copy())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	restarted := NewClient(pool)
	_, err = restarted.ClaimSessionQuiescence(t.Context())
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, restarted.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
	require.Equal(t, due, quiescenceDeadline(t, restarted, version))
	var work *SessionQuiescence
	require.Eventually(t, func() bool {
		work, err = restarted.ClaimSessionQuiescence(t.Context())
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, version, work.Version)
	_, err = client.ClaimSessionQuiescence(t.Context())
	require.ErrorIs(t, err, ErrNotFound, "restarts must not reissue a claimed boundary")
	snapshot := &SessionTaskSnapshot{Atespace: "team-a", URI: "snapshot", ContentScope: "DATA"}
	require.NoError(t, client.FinishSessionQuiescence(t.Context(), work, snapshot))
	require.NoError(t, restarted.FinishSessionQuiescence(t.Context(), work, snapshot))
	_, err = restarted.ClaimSessionQuiescence(t.Context())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSessionQuiescenceDeadlineClamp(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
		want  time.Duration
	}{
		{name: "negative", delay: -time.Minute},
		{name: "above maximum", delay: time.Hour, want: 30 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient(setupTestDB(t))
			session, task, version := quiescenceBoundaryFixture(t, client, a2a.TaskStateCompleted)
			before := databaseTime(t, client)
			require.NoError(t, client.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, test.delay))
			after := databaseTime(t, client)
			due := quiescenceDeadline(t, client, version)
			require.False(t, due.Before(before.Add(test.want)))
			require.False(t, due.After(after.Add(test.want)))
		})
	}
}
