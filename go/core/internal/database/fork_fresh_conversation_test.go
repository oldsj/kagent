package database

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

func TestForkSessionFreshConversation(t *testing.T) {
	t.Run("Given a ready checkpoint, when it is forked with a fresh conversation, assert that the fork is marked and copies no history", func(t *testing.T) {
		client, checkpointID := givenReadyCheckpoint(t)
		forkID := uuid.NewString()

		fork, created, err := client.ForkSession(t.Context(), checkpointID, "alice", "fresh-fork", forkID, true)

		require.NoError(t, err)
		require.True(t, created)
		require.True(t, fork.GetFreshConversation())
		stored, err := client.GetSession(t.Context(), forkID, "alice")
		require.NoError(t, err)
		require.True(t, stored.GetFreshConversation(), "the mode persists for the runtime to read")
		tasks, total, err := client.ListSessionTasks(t.Context(), forkID, "", a2a.TaskStateUnspecified, nil, 10, nil)
		require.NoError(t, err)
		require.Zero(t, total)
		require.Empty(t, tasks)
	})

	t.Run("Given a ready checkpoint, when it is forked by default, assert that the fork is unmarked and keeps its history", func(t *testing.T) {
		client, checkpointID := givenReadyCheckpoint(t)
		forkID := uuid.NewString()

		fork, _, err := client.ForkSession(t.Context(), checkpointID, "alice", "default-fork", forkID, false)

		require.NoError(t, err)
		require.False(t, fork.GetFreshConversation())
		_, total, err := client.ListSessionTasks(t.Context(), forkID, "", a2a.TaskStateUnspecified, nil, 10, nil)
		require.NoError(t, err)
		require.Equal(t, 1, total)
	})

	t.Run("Given a fresh fork, when the same request repeats, assert that it replays only in the same conversation mode", func(t *testing.T) {
		client, checkpointID := givenReadyCheckpoint(t)
		fork, _, err := client.ForkSession(t.Context(), checkpointID, "alice", "fresh-fork", uuid.NewString(), true)
		require.NoError(t, err)

		replayed, created, err := client.ForkSession(t.Context(), checkpointID, "alice", "fresh-fork", uuid.NewString(), true)
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, fork.GetId(), replayed.GetId())

		_, _, err = client.ForkSession(t.Context(), checkpointID, "alice", "fresh-fork", uuid.NewString(), false)
		require.ErrorIs(t, err, ErrIdempotencyConflict)
	})
}

// givenReadyCheckpoint returns a READY checkpoint of a completed one-turn Session owned by alice.
func givenReadyCheckpoint(t *testing.T) (*Client, string) {
	t.Helper()
	ctx := t.Context()
	client := NewClient(setupTestDB(t), "public")
	revision := RuntimeRevision{
		Revision: "revision-1", Namespace: "team-a", AgentName: "assistant", AgentUID: "template-uid",
		SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{Name: "assistant"}, EgressDestinations: []string{},
		ActorTemplateAtespace: "team-a", ActorTemplateName: "assistant-kagent-revision", ActorTemplateUID: "actor-template-uid",
	}
	require.NoError(t, client.UpsertAgentDefinition(ctx, AgentDefinition{
		Namespace: "team-a", AgentName: "assistant", AgentUID: "template-uid", DesiredRevision: revision.Revision,
	}))
	require.NoError(t, recordErr(client.RecordRuntimeRevision(ctx, revision, RecordRevisionOptions{Ready: true})))
	source, _, err := client.CreateSession(ctx, &apiv1.Session{
		Id: uuid.NewString(), Creator: "alice",
		Agent: &apiv1.AgentProvenance{Ref: &apiv1.ResourceReference{Namespace: "team-a", Name: "assistant"}},
	}, "source-request")
	require.NoError(t, err)
	_, err = markSessionReady(ctx, client, source.GetId(), "source.example")
	require.NoError(t, err)
	task := newSessionTask("task-1", "message-1")
	task.ContextID = source.GetContextId()
	task.History[0].ContextID, task.History[0].TaskID = source.GetContextId(), task.ID
	_, err = client.CreateRuntimeTask(ctx, source.GetId(), taskMutationHash("message-request-1"), task, "")
	require.NoError(t, err)
	task.Status.State = a2a.TaskStateCompleted
	require.NoError(t, saveRuntimeTask(t, client, source.GetId(), task, task,
		&SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", Fidelity: "SNAPSHOT_FIDELITY_VOLUMES"}))
	checkpoint, _, err := client.ReserveSessionCheckpoint(ctx, &apiv1.Checkpoint{Id: uuid.NewString(), SessionId: source.GetId(), HeadTaskId: "task-1"}, "alice", "checkpoint-request")
	require.NoError(t, err)
	_, err = client.FinalizeSessionCheckpoint(ctx, checkpoint.GetId(), "tag-uid-1", "s3://tags/checkpoint", "")
	require.NoError(t, err)
	return client, checkpoint.GetId()
}
