package checkpoint

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// ownedStore returns a checkpoint only to the user who owns it, as the database
// store does, and records which user each call acted for.
type ownedStore struct {
	*testStore
	owner string
	users []string
}

func (s *ownedStore) visible(userID string) error {
	s.users = append(s.users, userID)
	if userID != s.owner {
		return database.ErrNotFound
	}
	return nil
}

func (s *ownedStore) GetSession(ctx context.Context, id, userID string) (*apiv1alpha1.Session, error) {
	if err := s.visible(userID); err != nil {
		return nil, err
	}
	return s.testStore.GetSession(ctx, id, userID)
}

func (s *ownedStore) GetCheckpointSource(ctx context.Context, id, userID string) (*apiv1alpha1.Session, error) {
	if err := s.visible(userID); err != nil {
		return nil, err
	}
	return s.testStore.GetCheckpointSource(ctx, id, userID)
}

func (s *ownedStore) GetSessionCheckpoint(ctx context.Context, id, userID string) (*apiv1alpha1.Checkpoint, error) {
	if err := s.visible(userID); err != nil {
		return nil, err
	}
	return s.testStore.GetSessionCheckpoint(ctx, id, userID)
}

// ListSessionCheckpoints filters by owner in SQL, so another owner gets an empty
// page rather than an error.
func (s *ownedStore) ListSessionCheckpoints(ctx context.Context, sessionID, userID, after string, limit int) ([]*apiv1alpha1.Checkpoint, error) {
	if s.visible(userID) != nil {
		return nil, nil
	}
	return s.testStore.ListSessionCheckpoints(ctx, sessionID, userID, after, limit)
}

func (s *ownedStore) BeginDeleteSessionCheckpoint(ctx context.Context, id, userID string) (*database.SessionTaskSnapshot, string, error) {
	if err := s.visible(userID); err != nil {
		return nil, "", err
	}
	return s.testStore.BeginDeleteSessionCheckpoint(ctx, id, userID)
}

func (s *ownedStore) GetSessionCheckpointSnapshot(ctx context.Context, id, userID string) (*database.SessionTaskSnapshot, string, error) {
	if err := s.visible(userID); err != nil {
		return nil, "", err
	}
	return s.testStore.GetSessionCheckpointSnapshot(ctx, id, userID)
}

func (s *ownedStore) ForkSession(ctx context.Context, id, userID, requestID, sessionID string, fresh bool, credentials ...*apiv1alpha1.SessionCredential) (*apiv1alpha1.Session, bool, error) {
	if err := s.visible(userID); err != nil {
		return nil, false, err
	}
	return s.testStore.ForkSession(ctx, id, userID, requestID, sessionID, fresh, credentials...)
}

type mainloopSession struct{ userID string }

func (s mainloopSession) Principal() auth.Principal {
	return auth.Principal{User: auth.User{ID: s.userID}, Service: "mainloop"}
}

func mainloopFixture(t *testing.T, owner string) (*Service, *ownedStore, *testTags, *apiv1alpha1.Checkpoint) {
	t.Helper()
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &ownedStore{owner: owner, testStore: &testStore{
		prepared: checkpoint, tagUID: "tag-uid",
		snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: "DATA"},
	}}
	tags := &testTags{created: &ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.Id), Uid: "tag-uid"},
		Status:   &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshots/snapshot-1", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}},
	}}
	return NewService(store, policy, tags, &testWorkflow{}), store, tags, checkpoint
}

// The Mainloop service identity manages the checkpoints of the sessions it owns,
// and the store receives that identity as the owner to scope by.
func TestMainloopManagesItsOwnCheckpoints(t *testing.T) {
	service, store, _, checkpoint := mainloopFixture(t, "mainloop")
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{userID: "mainloop"})

	got, err := service.Get(ctx, checkpoint.Id)
	require.NoError(t, err)
	require.Equal(t, checkpoint.Id, got.Id)
	store.listed = []*apiv1alpha1.Checkpoint{checkpoint}
	_, err = service.List(ctx, ListRequest{SessionID: "018f47a2-4efb-7c21-a848-123456789abd"})
	require.NoError(t, err)
	_, err = service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abd", "create-request", "task-1")
	require.NoError(t, err)
	for _, fresh := range []bool{false, true} {
		fork, err := service.Fork(ctx, checkpoint.Id, "fork-request", fresh)
		require.NoError(t, err)
		require.Equal(t, "mainloop", fork.Creator)
		require.Equal(t, fresh, fork.FreshConversation)
		store.forked = nil
	}
	require.NoError(t, service.Delete(ctx, checkpoint.Id))
	require.NotEmpty(t, store.users)
	for _, user := range store.users {
		require.Equal(t, "mainloop", user)
	}
}

func TestMainloopCannotReachAnotherOwnersCheckpoints(t *testing.T) {
	service, store, tags, checkpoint := mainloopFixture(t, "someone-else")
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{userID: "mainloop"})

	_, err := service.Get(ctx, checkpoint.Id)
	require.Equal(t, serviceerrors.CodeNotFound, serviceerrors.CodeOf(err))
	listed, err := service.List(ctx, ListRequest{SessionID: "018f47a2-4efb-7c21-a848-123456789abd"})
	require.NoError(t, err)
	require.Empty(t, listed.Checkpoints)
	_, err = service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abd", "create-request", "task-1")
	require.Equal(t, serviceerrors.CodeNotFound, serviceerrors.CodeOf(err))
	_, err = service.Fork(ctx, checkpoint.Id, "fork-request", true)
	require.Equal(t, serviceerrors.CodeNotFound, serviceerrors.CodeOf(err))
	err = service.Delete(ctx, checkpoint.Id)
	require.Equal(t, serviceerrors.CodeNotFound, serviceerrors.CodeOf(err))
	require.Nil(t, store.forked)
	require.Zero(t, tags.deleteCalls)
	require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, checkpoint.State)
}

// Renaming stays outside the Mainloop surface even for its own checkpoints.
func TestMainloopCannotRenameCheckpoints(t *testing.T) {
	service, _, _, checkpoint := mainloopFixture(t, "mainloop")
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{userID: "mainloop"})
	_, err := service.Rename(ctx, checkpoint.Id, "renamed")
	require.Equal(t, serviceerrors.CodePermissionDenied, serviceerrors.CodeOf(err))
}

// The checkpoint calls follow the same Agent allowlist as Session operations. A
// fork of a checkpoint whose Agent was removed would otherwise create a Session
// Mainloop can neither see nor delete, pinning the checkpoint.
func TestMainloopCheckpointsFollowTheAgentAllowlist(t *testing.T) {
	service, store, tags, checkpoint := mainloopFixture(t, "mainloop")
	store.agent = &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "retired-agent"}
	store.listed = []*apiv1alpha1.Checkpoint{checkpoint}
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{userID: "mainloop"})
	denied := func(err error) { require.Equal(t, serviceerrors.CodePermissionDenied, serviceerrors.CodeOf(err)) }

	_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abd", "create-request", "task-1")
	denied(err)
	_, err = service.Get(ctx, checkpoint.Id)
	denied(err)
	_, err = service.List(ctx, ListRequest{SessionID: "018f47a2-4efb-7c21-a848-123456789abd"})
	denied(err)
	for _, fresh := range []bool{false, true} {
		_, err = service.Fork(ctx, checkpoint.Id, "fork-request", fresh)
		denied(err)
	}
	denied(service.Delete(ctx, checkpoint.Id))
	require.Nil(t, store.forked)
	require.False(t, store.deleted)
	require.Zero(t, tags.deleteCalls)
	require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, checkpoint.State)
}

// A fork's credentials follow the same operator-approved allowlist as
// CreateSession's, checked against the checkpoint's source Agent since Fork
// has no caller-supplied Agent reference of its own.
func TestMainloopForkCredentialsFollowTheApprovedAllowlist(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{
		Namespace: "kagent", Agents: []string{"mainloop-main"},
		Credentials: []controlauth.CredentialRule{{
			Namespace: "kagent", SecretNamePattern: "mainloop-token", Key: "value",
			Origin: "https://mcp.example.com", Header: "authorization", Purpose: "mcp",
		}},
	})
	require.NoError(t, err)
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &ownedStore{owner: "mainloop", testStore: &testStore{
		prepared: checkpoint, tagUID: "tag-uid",
		snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: "DATA"},
	}}
	tags := &testTags{created: &ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.Id), Uid: "tag-uid"},
		Status:   &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshots/snapshot-1", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}},
	}}
	service := NewService(store, policy, tags, &testWorkflow{})
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{userID: "mainloop"})

	denied := []*apiv1alpha1.SessionCredential{{Origin: "https://unapproved.example.com", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-token", Key: "value"}}}
	_, err = service.Fork(ctx, checkpoint.Id, "fork-request-denied", false, denied...)
	require.Equal(t, serviceerrors.CodePermissionDenied, serviceerrors.CodeOf(err))
	require.Nil(t, store.forked)

	allowed := []*apiv1alpha1.SessionCredential{{Origin: "https://mcp.example.com", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-token", Key: "value"}}}
	fork, err := service.Fork(ctx, checkpoint.Id, "fork-request-allowed", false, allowed...)
	require.NoError(t, err)
	require.Len(t, fork.GetCredentials(), 1)
	require.True(t, proto.Equal(allowed[0], fork.GetCredentials()[0]))
}
