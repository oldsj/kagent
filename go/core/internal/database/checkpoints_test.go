package database

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

// TestDeleteFailedCheckpointSucceeds confirms a checkpoint whose external snapshot
// tag never retained (FAILED) can still be deleted, the same as a READY one.
func TestDeleteFailedCheckpointSucceeds(t *testing.T) {
	client := NewClient(setupTestDB(t))
	ctx := t.Context()
	sessionFixture(t, client, ctx, "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "session")
	require.NoError(t, err)
	_, err = markSessionReady(ctx, client, session.GetId(), "runtime")
	require.NoError(t, err)

	task := newSessionTask("task", "message")
	task.ContextID = session.GetContextId()
	_, err = client.CreateRuntimeTask(ctx, session.GetId(), taskMutationHash("request"), task, "")
	require.NoError(t, err)
	task.Status.State = a2a.TaskStateCompleted
	require.NoError(t, saveRuntimeTask(t, client, session.GetId(), task, task,
		&SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/task", ContentScope: "DATA"}))

	checkpoint, _, err := client.ReserveSessionCheckpoint(ctx, &apiv1alpha1.Checkpoint{
		Id: uuid.NewString(), SessionId: session.GetId(), HeadTaskId: string(task.ID),
	}, "alice", "checkpoint")
	require.NoError(t, err)

	_, err = client.FinalizeSessionCheckpoint(ctx, checkpoint.GetId(), "", "", "snapshot tag could not be created")
	require.NoError(t, err)
	_, err = client.GetSessionCheckpoint(ctx, checkpoint.GetId(), "alice")
	require.ErrorIs(t, err, ErrNotFound, "a FAILED checkpoint does not read back as READY")

	snapshot, tagUID, err := client.BeginDeleteSessionCheckpoint(ctx, checkpoint.GetId(), "alice")
	require.NoError(t, err, "a FAILED checkpoint must be deletable like a READY one")
	require.Empty(t, tagUID, "a FAILED checkpoint never retained a tag")
	require.Equal(t, "team-a", snapshot.Atespace)

	require.NoError(t, client.DeleteSessionCheckpoint(ctx, checkpoint.GetId(), "alice"))
	_, _, err = client.GetSessionCheckpointSnapshot(ctx, checkpoint.GetId(), "alice")
	require.ErrorIs(t, err, ErrNotFound, "the deleted checkpoint row must be gone")
}
