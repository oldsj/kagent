package database

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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

// TestForkSessionCredentials confirms a fork receives exactly the credentials
// it is given, never the source Session's own bindings, with the same
// persistence, idempotency, and revision-allowlist behavior CreateSession
// applies.
func TestForkSessionCredentials(t *testing.T) {
	client := NewClient(setupTestDB(t))
	ctx := t.Context()
	sessionFixture(t, client, ctx, "team-a", "revision-1", "assistant", "kagent")
	original, err := client.GetRuntimeRevision(ctx, "revision-1")
	require.NoError(t, err)
	original.Revision = "revision-fork-credentials"
	original.ActorTemplateName = "fork-credential-template"
	original.EgressDestinations = []string{"http://echo.test.svc.cluster.local:80"}
	require.NoError(t, client.UpsertAgentDefinition(ctx, AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: original.AgentUID, DesiredRevision: original.Revision}))
	require.NoError(t, client.RecordRuntimeRevision(ctx, *original, true))

	sourceCredential := []*apiv1alpha1.SessionCredential{{Origin: "http://echo.test.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "source-tokens", Key: "one"}}}
	source := newSessionRequest(uuid.NewString(), "assistant", "kagent", "")
	source.Credentials = sourceCredential
	session, _, err := client.CreateSession(ctx, source, "source-request")
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
	_, err = client.FinalizeSessionCheckpoint(ctx, checkpoint.GetId(), "tag-uid", "s3://tags/checkpoint", "")
	require.NoError(t, err)

	forkCredentials := []*apiv1alpha1.SessionCredential{{Origin: "http://echo.test.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "fork-tokens", Key: "two"}}}
	fork, created, err := client.ForkSession(ctx, checkpoint.GetId(), "alice", "fork-request", uuid.NewString(), true, forkCredentials...)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, fork.GetCredentials(), 1)
	require.True(t, proto.Equal(forkCredentials[0], fork.GetCredentials()[0]), "fork must receive exactly the credentials it was given")
	require.False(t, proto.Equal(sourceCredential[0], fork.GetCredentials()[0]), "fork must not inherit the source Session's credentials")

	read, err := client.GetSessionByID(ctx, fork.GetId())
	require.NoError(t, err)
	require.True(t, proto.Equal(forkCredentials[0], read.GetCredentials()[0]), "credentials must persist before the fork is exposed")

	retry, created, err := client.ForkSession(ctx, checkpoint.GetId(), "alice", "fork-request", uuid.NewString(), true, forkCredentials...)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, fork.GetId(), retry.GetId())

	changed := proto.CloneOf(forkCredentials[0])
	changed.SecretRef.Key = "three"
	_, _, err = client.ForkSession(ctx, checkpoint.GetId(), "alice", "fork-request", uuid.NewString(), true, changed)
	require.ErrorIs(t, err, ErrIdempotencyConflict, "a retry with different credentials must conflict")

	_, _, err = client.ForkSession(ctx, checkpoint.GetId(), "alice", "fork-request", uuid.NewString(), true)
	require.ErrorIs(t, err, ErrIdempotencyConflict, "a retry without credentials must also conflict")

	disallowed := []*apiv1alpha1.SessionCredential{{Origin: "https://unlisted.example.com", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: "one"}}}
	_, _, err = client.ForkSession(ctx, checkpoint.GetId(), "alice", uuid.NewString(), uuid.NewString(), true, disallowed...)
	require.ErrorIs(t, err, ErrSessionCredentialNotAllowed)
}
