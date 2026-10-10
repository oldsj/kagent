package session

import (
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIdleLifecycleDoesNotOwnTaskPublication(t *testing.T) {
	for _, test := range []struct {
		name           string
		mutationFails  bool
		finishFailures int32
	}{
		{name: "snapshot succeeds"},
		{name: "snapshot outcome unknown", mutationFails: true},
		{name: "snapshot reference survives database retries", finishFailures: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			session, err := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
			require.NoError(t, err)
			_, err = base.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
			require.NoError(t, err)
			message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
			message.ContextID = session.ContextId
			task := a2a.NewSubmittedTask(message, message)
			task.Status.State = a2a.TaskStateCompleted
			hash := sha256.Sum256([]byte("completed"))
			version, err := store.CreateRuntimeTask(t.Context(), session.Id, hash[:], task, "")
			require.NoError(t, err)
			require.NoError(t, store.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))

			entered, release := make(chan struct{}), make(chan struct{})
			actors := &retryTestActors{lifecycleTestActors: base, beforeRead: func(ctx context.Context) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}}
			if test.mutationFails {
				actors.mutationErr = status.Error(codes.Unavailable, "lost suspend response")
			}
			// A new lifecycle worker discovers durable idle work without any
			// notification or participation from the task persistence service.
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			writes := &quiescenceRetryStore{lifecycleTestStore: store, failures: test.finishFailures}
			go func() {
				done <- NewActorWorkflow(writes, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Start(ctx)
			}()
			t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("idle work was not discovered")
			}
			visible, err := store.GetSettledSessionTask(t.Context(), session.Id, string(task.ID), nil)
			require.NoError(t, err)
			require.Equal(t, a2a.TaskStateCompleted, visible.Status.State)
			next := a2a.NewSubmittedTask(message, message)
			_, err = store.CreateRuntimeTask(t.Context(), session.Id, hash[:], next, "")
			require.ErrorIs(t, err, database.ErrFailedPrecondition)
			checkpoint := &apiv1alpha1.Checkpoint{Id: uuid.NewString(), SessionId: session.Id, HeadTaskId: string(task.ID)}
			_, _, err = store.ReserveSessionCheckpoint(t.Context(), checkpoint, "alice", "checkpoint")
			require.ErrorIs(t, err, database.ErrFailedPrecondition)
			close(release)

			if test.mutationFails {
				require.Eventually(t, func() bool { return actors.mutations.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
				visible, err = store.GetSettledSessionTask(t.Context(), session.Id, string(task.ID), nil)
				require.NoError(t, err)
				require.Equal(t, a2a.TaskStateCompleted, visible.Status.State)
				_, err = store.ClaimSessionQuiescence(t.Context())
				require.ErrorIs(t, err, database.ErrNotFound, "an uncertain suspension cannot be reassigned")
			} else {
				require.Eventually(t, func() bool {
					_, snapshot, err := store.ReserveSessionCheckpoint(t.Context(), checkpoint, "alice", "checkpoint")
					return err == nil && snapshot.URI == "s3://snapshots/snapshot-1"
				}, 5*time.Second, 10*time.Millisecond)
				require.EqualValues(t, 1, actors.mutations.Load(), "database retries must not suspend the actor again")
				require.Equal(t, test.finishFailures+1, writes.attempts.Load())
			}
		})
	}
}

type quiescenceRetryStore struct {
	*lifecycleTestStore
	failures int32
	attempts atomic.Int32
}

func (s *quiescenceRetryStore) FinishSessionQuiescence(ctx context.Context, work *database.SessionQuiescence, snapshot *database.SessionTaskSnapshot) error {
	if s.attempts.Add(1) <= s.failures {
		return status.Error(codes.Unavailable, "database unavailable")
	}
	return s.Client.FinishSessionQuiescence(ctx, work, snapshot)
}

func TestIdleQuiescenceRecoveryAfterScopeRejection(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "matching Full snapshot recovers"
		if mismatch {
			name = "mismatched snapshot remains fenced"
		}
		t.Run(name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}, snapshotScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL}
			template, err := base.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
			require.NoError(t, err)
			template.SnapshotConfig.OnCommit = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
			base.template = template
			actors := &retryTestActors{lifecycleTestActors: base}
			workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			session, err = workflow.Create(t.Context(), session)
			require.NoError(t, err)
			_, err = actors.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
			require.NoError(t, err)
			message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("done"))
			message.ContextID = session.ContextId
			task := a2a.NewSubmittedTask(message, message)
			task.Status.State = a2a.TaskStateCompleted
			hash := sha256.Sum256([]byte("completed"))
			version, err := store.CreateRuntimeTask(t.Context(), session.Id, hash[:], task, "")
			require.NoError(t, err)
			require.NoError(t, store.SettleSessionTask(t.Context(), session.Id, string(task.ID), version, 0))
			oldClaim, err := store.ClaimSessionQuiescence(t.Context())
			require.NoError(t, err)
			// Reproduce the old controller's failure: Suspend succeeded, but its
			// FULL response was rejected and the original claim was never finished.
			actor, err := actors.SuspendActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
			require.NoError(t, err)
			require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, actor.GetStatus().GetExternalSnapshot().GetContentScope())
			_, err = workflow.Suspend(t.Context(), session)
			require.ErrorIs(t, err, database.ErrFailedPrecondition)
			_, err = store.ClaimSessionQuiescence(t.Context())
			require.ErrorIs(t, err, database.ErrNotFound, "restarting a worker alone must not steal uncertain work")
			if mismatch {
				base.mu.Lock()
				base.actors[actorKey("team-a", fixtureActorName(t, store, session.Id))].Status.ExternalSnapshot.ContentScope = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA
				base.mu.Unlock()
			}
			// Operator recovery after all old controller workers have stopped.
			// The exact version/executor predicate cannot release another claim.
			tag, err := store.pool.Exec(t.Context(), `
				UPDATE session_task_event SET quiescence_executor_id = NULL
				WHERE sequence = $1 AND quiescence_executor_id = $2
				  AND published AND quiescence_pending = TRUE
			`, oldClaim.Version, oldClaim.ExecutorID)
			require.NoError(t, err)
			require.EqualValues(t, 1, tag.RowsAffected())
			newClaim, err := store.ClaimSessionQuiescence(t.Context())
			require.NoError(t, err)
			require.NotEqual(t, oldClaim.ExecutorID, newClaim.ExecutorID)
			mutations := actors.mutations.Load()
			// A fresh worker uses the suspended result, with no second mutation.
			NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").quiesceIdleSession(t.Context(), newClaim)
			require.Equal(t, mutations, actors.mutations.Load())
			checkpoint, boundary, err := store.ReserveSessionCheckpoint(t.Context(), &apiv1alpha1.Checkpoint{Id: uuid.NewString(), SessionId: session.Id, HeadTaskId: string(task.ID)}, "alice", "checkpoint")
			if mismatch {
				require.ErrorIs(t, err, database.ErrFailedPrecondition)
				_, err = workflow.Suspend(t.Context(), session)
				require.ErrorIs(t, err, database.ErrFailedPrecondition)
				return
			}
			require.NoError(t, err)
			require.Equal(t, string(task.ID), checkpoint.HeadTaskId)
			require.EqualValues(t, version, checkpoint.HistorySequence)
			require.Equal(t, "team-a", boundary.Atespace)
			require.Equal(t, "s3://snapshots/snapshot-1", boundary.URI)
			require.Equal(t, "FULL", boundary.ContentScope)
			// The runbook requires the original task and event to retain the exact
			// snapshot, not merely pending=false (which supersession also sets).
			var recordedTask string
			var recordedVersion, historySequence int64
			var retainedExecutor uuid.UUID
			require.NoError(t, store.pool.QueryRow(t.Context(), `
				SELECT e.task_id, e.sequence, t.history_sequence, e.quiescence_executor_id
				FROM session_record s
				JOIN session_task_event e ON e.history_id = s.history_id
				JOIN session_task t ON t.history_id = e.history_id AND t.id = e.task_id
				WHERE s.id = $1::uuid
				  AND e.task_id = $2 AND e.sequence = $3::bigint
				  AND e.published AND e.quiescence_pending = FALSE
				  AND t.history_sequence = e.sequence
				  AND e.snapshot_atespace = $4 AND t.snapshot_atespace = e.snapshot_atespace
				  AND e.snapshot_uri = $5 AND t.snapshot_uri = e.snapshot_uri
				  AND e.snapshot_content_scope = $6
				  AND t.snapshot_content_scope = e.snapshot_content_scope
			`, session.Id, string(task.ID), version, boundary.Atespace, boundary.URI, boundary.ContentScope).
				Scan(&recordedTask, &recordedVersion, &historySequence, &retainedExecutor))
			require.Equal(t, string(task.ID), recordedTask)
			require.Equal(t, version, recordedVersion)
			require.Equal(t, version, historySequence)
			require.Equal(t, newClaim.ExecutorID, retainedExecutor)
			// Reserving a checkpoint fences explicit lifecycle until finalized.
			checkpoint, err = store.FinalizeSessionCheckpoint(t.Context(), checkpoint.Id, "tag-uid", "s3://retained/snapshot", "")
			require.NoError(t, err)
			snapshot, _, err := store.GetSessionCheckpointSnapshot(t.Context(), checkpoint.Id, "alice")
			require.NoError(t, err)
			require.Equal(t, "FULL", snapshot.ContentScope)
			_, err = workflow.Suspend(t.Context(), session)
			require.NoError(t, err, "explicit suspension becomes available again")
		})
	}
}
