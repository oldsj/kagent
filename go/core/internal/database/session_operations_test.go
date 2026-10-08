package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func observedSuspensionFixture(t *testing.T) (*Client, *SessionOperation, *RuntimeGeneration) {
	t.Helper()
	client, session, generation := generationFixture(t)
	activateGeneration(t, client, generation)
	creation, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
	require.NoError(t, err)
	executor := uuid.New()
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, creation.ID, executor)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, creation.ID, executor, "runtime.example", generation.ActorUID, "")
	require.NoError(t, err)
	operation, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	return client, operation, generation
}

func TestFinishObservedSessionSuspension(t *testing.T) {
	client, operation, generation := observedSuspensionFixture(t)
	_, err := client.UpdateSessionName(t.Context(), operation.Instance.Id, "alice", "renamed during observation")
	require.NoError(t, err)
	finished, err := client.FinishObservedSessionSuspension(t.Context(), operation.Instance.Id, operation.ID, operation.Instance.PreparedRevision, generation.ActorUID)
	require.NoError(t, err)
	require.Equal(t, "renamed during observation", finished.Name)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, finished.State)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, finished.Operation)
	require.Equal(t, operation.Instance.A2AAuthority, finished.A2AAuthority)
	// Reconcile a lost completion reply through admission, without a new claim.
	observed, err := client.BeginSessionOperation(t.Context(), finished.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	require.Equal(t, operation.ID, observed.ID)
	require.Equal(t, uuid.Nil, observed.ExecutorID)
	require.True(t, proto.Equal(finished, observed.Instance))
	claimed, err := client.ClaimSessionOperation(t.Context(), finished.Id, operation.ID, uuid.New())
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = client.FinishObservedSessionSuspension(t.Context(), finished.Id, operation.ID, finished.PreparedRevision, generation.ActorUID)
	require.ErrorIs(t, err, ErrConflict, "direct stale completion is not new authority")
}

func TestObservedSessionSuspensionRejectsChangedAuthority(t *testing.T) {
	for _, name := range []string{"nil operation", "stale operation", "wrong revision", "empty revision", "wrong Actor UID", "empty Actor UID", "claimed", "released claim", "superseded by delete", "revoked generation", "Session UID changed"} {
		t.Run(name, func(t *testing.T) {
			client, operation, generation := observedSuspensionFixture(t)
			id, revision, uid := operation.ID, operation.Instance.PreparedRevision, generation.ActorUID
			switch name {
			case "nil operation":
				id = uuid.Nil
			case "stale operation":
				id = uuid.New()
			case "wrong revision":
				revision = "different-revision"
			case "empty revision":
				revision = ""
			case "wrong Actor UID":
				uid = "replacement-actor"
			case "empty Actor UID":
				uid = ""
			case "claimed", "released claim":
				executor := uuid.New()
				claimed, err := client.ClaimSessionOperation(t.Context(), operation.Instance.Id, id, executor)
				require.NoError(t, err)
				require.True(t, claimed)
				if name == "released claim" {
					require.NoError(t, client.ReleaseRuntimeOperation(t.Context(), operation.Instance.Id, id, executor))
				}
			case "superseded by delete":
				deletion, err := client.BeginSessionOperation(t.Context(), operation.Instance.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
				require.NoError(t, err)
				id = deletion.ID // Even a current ID cannot finish the wrong kind.
			case "revoked generation":
				require.NoError(t, client.RevokeRuntimeGeneration(t.Context(), operation.Instance.Id))
			case "Session UID changed":
				_, err := client.db.Exec(t.Context(), `UPDATE session SET actor_uid = 'replacement-actor' WHERE id = $1`, operation.Instance.Id)
				require.NoError(t, err)
			}
			kind := apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND
			if name == "superseded by delete" {
				kind = apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE
			}
			before, err := client.BeginSessionOperation(t.Context(), operation.Instance.Id, kind)
			require.NoError(t, err)
			_, err = client.FinishObservedSessionSuspension(t.Context(), operation.Instance.Id, id, revision, uid)
			require.ErrorIs(t, err, ErrConflict)
			after, err := client.GetSessionOperation(t.Context(), operation.Instance.Id, before.ID)
			require.NoError(t, err)
			require.Equal(t, before.ID, after.ID)
			require.Equal(t, before.ExecutorID, after.ExecutorID)
			require.Equal(t, before.SourceCheckpointID, after.SourceCheckpointID)
			require.True(t, proto.Equal(before.Instance, after.Instance), "a rejected observation must retain lifecycle data, including after a late SQL predicate failure")
		})
	}
}

func TestObservedSessionSuspensionRollsBackDatabaseFailure(t *testing.T) {
	client, operation, generation := observedSuspensionFixture(t)
	_, err := client.db.Exec(t.Context(), `
		CREATE FUNCTION reject_observed_suspension() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected Session write failure'; END $$;
		CREATE TRIGGER reject_observed_suspension BEFORE UPDATE ON session
		FOR EACH ROW EXECUTE FUNCTION reject_observed_suspension()
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.db.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_observed_suspension ON session; DROP FUNCTION IF EXISTS reject_observed_suspension()`)
	})
	_, err = client.FinishObservedSessionSuspension(t.Context(), operation.Instance.Id, operation.ID, operation.Instance.PreparedRevision, generation.ActorUID)
	require.ErrorContains(t, err, "injected Session write failure")
	held, err := client.GetSessionOperation(t.Context(), operation.Instance.Id, operation.ID)
	require.NoError(t, err)
	require.Equal(t, operation.ID, held.ID)
	require.Equal(t, uuid.Nil, held.ExecutorID)
	require.Equal(t, operation.SourceCheckpointID, held.SourceCheckpointID)
	require.True(t, proto.Equal(operation.Instance, held.Instance), "the earlier runtime lifecycle update must roll back with the Session write")
	_, err = client.db.Exec(t.Context(), `DROP TRIGGER reject_observed_suspension ON session; DROP FUNCTION reject_observed_suspension()`)
	require.NoError(t, err)
	_, err = client.FinishObservedSessionSuspension(t.Context(), operation.Instance.Id, operation.ID, operation.Instance.PreparedRevision, generation.ActorUID)
	require.NoError(t, err, "rollback leaves the same never-claimed observation retryable")
}

func TestObservedSessionSuspensionContendsWithClaim(t *testing.T) {
	for _, first := range []string{"claim", "observation"} {
		t.Run(first, func(t *testing.T) {
			client, operation, generation := observedSuspensionFixture(t)
			tx, err := sharedDB.Begin(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			_, err = lockSession(t.Context(), tx, operation.Instance.Id)
			require.NoError(t, err)
			executor := uuid.New()
			type claimResult struct {
				won bool
				err error
			}
			claims, observations := make(chan claimResult, 1), make(chan error, 1)
			claim := func() {
				won, err := client.ClaimSessionOperation(t.Context(), operation.Instance.Id, operation.ID, executor)
				claims <- claimResult{won, err}
			}
			observe := func() {
				_, err := client.FinishObservedSessionSuspension(t.Context(), operation.Instance.Id, operation.ID, operation.Instance.PreparedRevision, generation.ActorUID)
				observations <- err
			}
			waitBlocked := func(count int) {
				t.Helper()
				require.Eventually(t, func() bool {
					var blocked int
					err := sharedDB.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&blocked)
					return err == nil && blocked >= count
				}, 5*time.Second, 10*time.Millisecond, "observe actual PostgreSQL contention")
			}
			if first == "claim" {
				go claim()
				waitBlocked(1)
				go observe()
			} else {
				go observe()
				waitBlocked(1)
				go claim()
			}
			waitBlocked(2)
			require.NoError(t, tx.Commit(t.Context()))
			claimed, observedErr := <-claims, <-observations
			require.NoError(t, claimed.err)
			current, err := client.GetSessionOperation(t.Context(), operation.Instance.Id, operation.ID)
			require.NoError(t, err)
			if claimed.won {
				require.ErrorIs(t, observedErr, ErrConflict)
				require.Equal(t, executor, current.ExecutorID)
				require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, current.Instance.Operation)
				// Existing claimed completion still requires the winning executor.
				_, err = client.FinishSessionOperation(t.Context(), operation.Instance.Id, operation.ID, uuid.New(), "", generation.ActorUID, "")
				require.ErrorIs(t, err, ErrConflict)
				_, err = client.FinishSessionOperation(t.Context(), operation.Instance.Id, operation.ID, executor, "", generation.ActorUID, "")
				require.NoError(t, err)
			} else {
				require.NoError(t, observedErr)
				require.Equal(t, uuid.Nil, current.ExecutorID)
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, current.Instance.State)
				require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.Instance.Operation)
			}
		})
	}
}

func TestSessionOperationGenerationAndTombstone(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	create, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE, create.Instance.Operation)
	creator := uuid.New()
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, create.ID, creator)
	require.NoError(t, err)
	require.True(t, claimed)
	ready, err := client.FinishSessionOperation(t.Context(), session.Id, create.ID, creator, "runtime.example", "actor-uid", "")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, ready.State)
	_, err = client.GetSessionForRuntime(t.Context(), session.Id, "actor-uid")
	require.NoError(t, err)
	_, err = client.GetSessionForRuntime(t.Context(), session.Id, "replacement-uid")
	require.ErrorIs(t, err, ErrNotFound)

	suspend, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, suspend.Instance.Operation)
	executor := uuid.New()
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, suspend.ID, executor)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = client.UpdateSessionName(t.Context(), session.Id, "alice", "renamed during suspend")
	require.NoError(t, err)
	suspended, err := client.FinishSessionOperation(t.Context(), session.Id, suspend.ID, executor, "", "", "")
	require.NoError(t, err)
	require.Equal(t, "renamed during suspend", suspended.Name)

	resume, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME, resume.Instance.Operation)
	resumer := uuid.New()
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, resume.ID, resumer)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, resume.ID, resumer, "", "replacement-uid", "")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, resume.ID, resumer, "", "", "")
	require.NoError(t, err)
	// The lifecycle state is READY again; that does not restore the old authority.
	_, err = client.FinishSessionOperation(t.Context(), session.Id, create.ID, creator, "obsolete.example", "actor-uid", "")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.GetSessionOperation(t.Context(), session.Id, suspend.ID)
	require.ErrorIs(t, err, ErrConflict)

	deletion, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE, deletion.Instance.Operation)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING, deletion.Instance.State)
	deleter := uuid.New()
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, deletion.ID, deleter)
	require.NoError(t, err)
	require.True(t, claimed)
	deleted, err := client.FinishSessionOperation(t.Context(), session.Id, deletion.ID, deleter, "", "", "")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, deleted.State)
	require.Empty(t, deleted.PreparedRevision)
	_, err = client.GetSessionByID(t.Context(), session.Id)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = client.GetSessionOperation(t.Context(), session.Id, create.ID)
	require.ErrorIs(t, err, ErrConflict)
	current, err := client.GetSessionOperation(t.Context(), session.Id, deletion.ID)
	require.NoError(t, err)
	require.Equal(t, deleted.State, current.Instance.State)
	current, err = client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.NoError(t, err)
	require.Equal(t, deletion.ID, current.ID)
	require.Equal(t, deleted.State, current.Instance.State)
}

func TestSessionOperationClaimsAndPreparationRecovery(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	session, err = markSessionReady(t.Context(), client, session.Id, "runtime.example")
	require.NoError(t, err)
	first, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	// Success requires a claim, even when there is no competing executor.
	_, err = client.FinishSessionOperation(t.Context(), session.Id, first.ID, uuid.Nil, "", "", "")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, first.ID, uuid.Nil, "", "", "preparation unavailable")
	require.NoError(t, err)
	second, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, first.ID, uuid.New())
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, first.ID, uuid.Nil, "", "", "late failure")
	require.ErrorIs(t, err, ErrConflict)

	type outcome struct {
		executor uuid.UUID
		claimed  bool
		err      error
	}
	results := make(chan outcome, 8)
	for range cap(results) {
		go func() {
			executor := uuid.New()
			claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, second.ID, executor)
			results <- outcome{executor, claimed, err}
		}()
	}
	var winner uuid.UUID
	for range cap(results) {
		result := <-results
		require.NoError(t, result.err)
		if result.claimed {
			require.Equal(t, uuid.Nil, winner, "only one replica may authorize runtime work")
			winner = result.executor
		}
	}
	require.NotEqual(t, uuid.Nil, winner)
	joined, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	require.Equal(t, winner, joined.ExecutorID)
	require.Equal(t, second.Instance.Operation, joined.Instance.Operation)
	_, err = client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, second.ID, uuid.New(), "", "", "")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, second.ID, uuid.Nil, "", "", "cannot release issued work")
	require.ErrorIs(t, err, ErrConflict)
	// Even the claiming executor cannot release possibly issued work as a failure.
	_, err = client.FinishSessionOperation(t.Context(), session.Id, second.ID, winner, "", "", "runtime outcome unknown")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, second.ID, winner, "", "", "")
	require.NoError(t, err)
}

func TestDeleteSupersedesOnlyUnissuedCreation(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	creation, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
	require.NoError(t, err)
	deletion, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.NoError(t, err)
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, creation.ID, uuid.New())
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = client.GetSessionOperation(t.Context(), session.Id, creation.ID)
	require.ErrorIs(t, err, ErrConflict)
	executor := uuid.New()
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, deletion.ID, executor)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, deletion.ID, executor, "", "", "")
	require.NoError(t, err)
	_, err = client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = client.GetSessionOperation(t.Context(), session.Id, uuid.New())
	require.ErrorIs(t, err, ErrConflict)
}

func TestLifecycleABARejectsOldClaimAndCompletion(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	_, err = markSessionReady(t.Context(), client, session.Id, "runtime.example")
	require.NoError(t, err)
	var first *SessionOperation
	executor := uuid.New()
	for _, kind := range []apiv1alpha1.RuntimeOperation{
		apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND,
		apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME,
	} {
		operation, err := client.BeginSessionOperation(t.Context(), session.Id, kind)
		require.NoError(t, err)
		if first == nil {
			first = operation
		}
		claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, operation.ID, executor)
		require.NoError(t, err)
		require.True(t, claimed)
		_, err = client.FinishSessionOperation(t.Context(), session.Id, operation.ID, executor, "", "", "")
		require.NoError(t, err)
	}
	later, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, later.ID)
	require.Equal(t, first.Instance.State, later.Instance.State)
	require.Equal(t, first.Instance.Operation, later.Instance.Operation)
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, first.ID, uuid.New())
	require.NoError(t, err)
	require.False(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, first.ID, executor, "", "", "")
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.GetSessionOperation(t.Context(), session.Id, first.ID)
	require.ErrorIs(t, err, ErrConflict)
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, later.ID, executor)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestLifecycleObservationUsesCurrentFields(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	operation, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
	require.NoError(t, err)
	executor := uuid.New()
	claimed, err := client.ClaimSessionOperation(t.Context(), session.Id, operation.ID, executor)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = client.FinishSessionOperation(t.Context(), session.Id, operation.ID, executor, "runtime.example", "actor-uid", "")
	require.NoError(t, err)
	renamed, err := client.UpdateSessionName(t.Context(), session.Id, "alice", "current name")
	require.NoError(t, err)
	current, err := client.GetSessionOperation(t.Context(), session.Id, operation.ID)
	require.NoError(t, err)
	require.Equal(t, renamed.Name, current.Instance.Name)
	// Already at target is successful without needing a previous Resume receipt.
	resumed, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME)
	require.NoError(t, err)
	require.Equal(t, renamed.Name, resumed.Instance.Name)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, resumed.Instance.Operation)
	claimed, err = client.ClaimSessionOperation(t.Context(), session.Id, operation.ID, uuid.New())
	require.NoError(t, err)
	require.False(t, claimed, "completion must not become executable when its claim is cleared")
}
