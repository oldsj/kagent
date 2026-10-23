package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type testSession struct{ userID string }

func (s testSession) Principal() auth.Principal { return auth.Principal{User: auth.User{ID: s.userID}} }

type testAuthorizer struct{}

func (testAuthorizer) Check(context.Context, auth.Principal, auth.Verb, auth.Resource) error {
	return nil
}

type testStore struct {
	prepared    *apiv1alpha1.Checkpoint
	snapshot    *database.SessionTaskSnapshot
	tagUID      string
	snapshotErr error
	forked      *apiv1alpha1.Session
	// forkCredentials is what the most recent ForkSession call received.
	forkCredentials []*apiv1alpha1.SessionCredential
	// forkErr, if set, is returned by ForkSession instead of creating a fork.
	forkErr        error
	failed         string
	deleted        bool
	finalizeErr    error
	reserveErr     error
	beginDeleteErr error
	scope          string
	// agent is the Agent the checkpoint's source Session runs; nil means the
	// allowlisted one the Mainloop fixtures use.
	agent *apiv1alpha1.ResourceReference
	// listed is what ListSessionCheckpoints returns.
	listed []*apiv1alpha1.Checkpoint
}

func (s *testStore) source(id, userID string) *apiv1alpha1.Session {
	agent := s.agent
	if agent == nil {
		agent = &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}
	}
	return &apiv1alpha1.Session{Id: id, Creator: userID, Agent: agent}
}

func (s *testStore) GetSession(_ context.Context, id, userID string) (*apiv1alpha1.Session, error) {
	return s.source(id, userID), nil
}

func (s *testStore) GetCheckpointSource(_ context.Context, id, userID string) (*apiv1alpha1.Session, error) {
	return s.source(id, userID), nil
}

func (s *testStore) ReserveSessionCheckpoint(_ context.Context, checkpoint *apiv1alpha1.Checkpoint, _, _ string) (*apiv1alpha1.Checkpoint, *database.SessionTaskSnapshot, error) {
	if s.reserveErr != nil {
		return nil, nil, s.reserveErr
	}
	if s.prepared != nil {
		return s.prepared, s.snapshot, nil
	}
	checkpoint.HeadTaskId = "task-1"
	checkpoint.HistorySequence = 7
	scope := s.scope
	if scope == "" {
		scope = "DATA"
	}
	s.snapshot = &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: scope}
	checkpoint.State = apiv1alpha1.CheckpointState_CHECKPOINT_STATE_CREATING
	checkpoint.CreatedAt = timestamppb.Now()
	s.prepared = checkpoint
	return checkpoint, s.snapshot, nil
}

func (s *testStore) FinalizeSessionCheckpoint(_ context.Context, _ string, tagUID, snapshotURI, failure string) (*apiv1alpha1.Checkpoint, error) {
	if s.finalizeErr != nil {
		return nil, s.finalizeErr
	}
	if failure != "" {
		s.prepared.State, s.failed = apiv1alpha1.CheckpointState_CHECKPOINT_STATE_FAILED, failure
	} else {
		s.prepared.State, s.tagUID = apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, tagUID
		s.snapshot.URI = snapshotURI
	}
	return s.prepared, nil
}

func (s *testStore) GetSessionCheckpoint(context.Context, string, string) (*apiv1alpha1.Checkpoint, error) {
	if s.prepared == nil {
		return nil, database.ErrNotFound
	}
	return s.prepared, nil
}

func (s *testStore) GetSessionCheckpointSnapshot(context.Context, string, string) (*database.SessionTaskSnapshot, string, error) {
	if s.snapshotErr != nil {
		return nil, "", s.snapshotErr
	}
	if s.prepared == nil {
		return nil, "", database.ErrNotFound
	}
	return s.snapshot, s.tagUID, nil
}

func (s *testStore) ListSessionCheckpoints(context.Context, string, string, string, int) ([]*apiv1alpha1.Checkpoint, error) {
	return s.listed, nil
}

func (s *testStore) BeginDeleteSessionCheckpoint(context.Context, string, string) (*database.SessionTaskSnapshot, string, error) {
	if s.beginDeleteErr != nil {
		return nil, "", s.beginDeleteErr
	}
	if s.prepared == nil {
		return nil, "", database.ErrNotFound
	}
	s.prepared.State = apiv1alpha1.CheckpointState_CHECKPOINT_STATE_DELETING
	return s.snapshot, s.tagUID, nil
}

func (s *testStore) DeleteSessionCheckpoint(context.Context, string, string) error {
	s.deleted = true
	return nil
}

func (s *testStore) UpdateCheckpointName(_ context.Context, _, _, name string) (*apiv1alpha1.Checkpoint, error) {
	if s.prepared == nil {
		return nil, database.ErrNotFound
	}
	s.prepared.Name = name
	return s.prepared, nil
}

func (s *testStore) ForkSession(_ context.Context, _ string, userID, _ string, sessionID string, fresh bool, credentials ...*apiv1alpha1.SessionCredential) (*apiv1alpha1.Session, bool, error) {
	s.forkCredentials = credentials
	if s.forkErr != nil {
		return nil, false, s.forkErr
	}
	if s.forked == nil {
		s.forked = &apiv1alpha1.Session{
			Id: sessionID, Creator: userID, FreshConversation: fresh, Credentials: credentials,
			State: apiv1alpha1.RuntimeState_RUNTIME_STATE_CREATING,
		}
		return s.forked, true, nil
	}
	return s.forked, false, nil
}

type testWorkflow struct {
	session *apiv1alpha1.Session
}

func (w *testWorkflow) Create(_ context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	w.session = session
	session.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_READY
	return session, nil
}

type testTags struct {
	createCalls            int
	snapshotURI            string
	snapshotURIAfterCreate string
	actorUIDAfterCreate    string
	created                *ateapipb.Tag
	deleteCalls            int
	deletedUID             string
	getErr                 error
	createErr              error
	deleteErr              error
	actorErr               error
	mutateTag              func(*ateapipb.Tag)
	scope                  ateapipb.SnapshotContentScope
}

func (t *testTags) contentScope() ateapipb.SnapshotContentScope {
	if t.scope == ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_UNSPECIFIED {
		return ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA
	}
	return t.scope
}

func (t *testTags) GetActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	if t.actorErr != nil {
		return nil, t.actorErr
	}
	uri := t.snapshotURI
	if t.created != nil && t.snapshotURIAfterCreate != "" {
		uri = t.snapshotURIAfterCreate
	}
	scope := t.contentScope()
	actorUID := "actor-uid"
	if t.created != nil && t.actorUIDAfterCreate != "" {
		actorUID = t.actorUIDAfterCreate
	}
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: actorUID},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
			ExternalSnapshot: &ateapipb.ExternalSnapshot{SnapshotUri: uri, ContentScope: scope, ActorTemplateUid: "template-uid"}},
	}, nil
}

func (t *testTags) GetTag(context.Context, string, string) (*ateapipb.Tag, error) {
	if t.getErr != nil {
		return nil, t.getErr
	}
	if t.created == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	return t.created, nil
}

func (t *testTags) CreateTag(_ context.Context, atespace, name, actorName string) (*ateapipb.Tag, error) {
	t.createCalls++
	if t.created != nil {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	t.created = &ateapipb.Tag{
		Metadata:    &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: "tag-uid"},
		SourceActor: &ateapipb.ObjectRef{Atespace: atespace, Name: actorName},
		Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
		Status: &ateapipb.TagStatus{ActorTemplateUid: "template-uid",
			Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://tags/checkpoint", ContentScope: t.contentScope()}},
	}
	if t.mutateTag != nil {
		t.mutateTag(t.created)
	}
	return t.created, t.createErr
}

func (t *testTags) DeleteTag(_ context.Context, _, _, uid string) error {
	t.deleteCalls++
	t.deletedUID = uid
	if t.deleteErr != nil {
		return t.deleteErr
	}
	t.created = nil
	return nil
}

func TestCreatePreservesStoreConflictReason(t *testing.T) {
	for _, cause := range []error{database.ErrConflict, database.ErrFailedPrecondition} {
		t.Run(cause.Error(), func(t *testing.T) {
			storeErr := fmt.Errorf("Session cannot checkpoint in its current state: %w", cause)
			service := NewService(&testStore{reserveErr: storeErr}, testAuthorizer{}, nil, nil)
			ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
			_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
			require.Equal(t, serviceerrors.CodeFailedPrecondition, serviceerrors.CodeOf(err))
			require.Equal(t, storeErr.Error(), serviceerrors.MessageOf(err))
			require.ErrorIs(t, err, cause)
		})
	}
}

func TestCreateDistinguishesPendingSnapshotFromAdvancedConversation(t *testing.T) {
	for _, test := range []struct {
		err    error
		reason string
	}{
		{database.ErrSnapshotPending, "KAGENT_CHECKPOINT_SNAPSHOT_PENDING"},
		{database.ErrCheckpointAdvanced, "KAGENT_CHECKPOINT_CONVERSATION_ADVANCED"},
	} {
		service := NewService(&testStore{reserveErr: test.err}, testAuthorizer{}, &testTags{}, &testWorkflow{})
		ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
		_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request", "task-a")
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		details := status.Convert(err).Details()
		require.Len(t, details, 1)
		info := details[0].(*errdetails.ErrorInfo)
		require.Equal(t, test.reason, info.Reason)
		require.Equal(t, "kagent.dev", info.Domain)
	}
}

func TestRenameValidatesBeforeReachingTheStore(t *testing.T) {
	store := &testStore{}
	service := NewService(store, testAuthorizer{}, nil, nil)
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
	const checkpointID = "018f47a2-4efb-7c21-a848-123456789abc"

	_, err := service.Rename(ctx, "not-a-uuid", "Before the detour")
	require.Equal(t, serviceerrors.CodeInvalidArgument, serviceerrors.CodeOf(err))

	_, err = service.Rename(ctx, checkpointID, "Before the detour")
	require.Equal(t, serviceerrors.CodeNotFound, serviceerrors.CodeOf(err))

	store.prepared = &apiv1alpha1.Checkpoint{Id: checkpointID}
	renamed, err := service.Rename(ctx, checkpointID, "Before the detour")
	require.NoError(t, err)
	require.Equal(t, "Before the detour", renamed.GetName())
}

func TestCreateTagsRecordedSnapshotBoundary(t *testing.T) {
	store := &testStore{snapshotErr: fmt.Errorf("creation must use the reserved snapshot")}
	tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1"}
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(context.Background(), testSession{userID: "alice"})

	checkpoint, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.GetHeadTaskId() != "task-1" || checkpoint.GetHistorySequence() != 7 || checkpoint.GetState().String() != "CHECKPOINT_STATE_READY" {
		t.Fatalf("unexpected checkpoint: %+v", checkpoint)
	}
	if tags.created.GetSourceActor().GetName() != "session-"+checkpoint.GetSessionId()+"-0123456789abcdef" ||
		store.snapshot.URI != "s3://tags/checkpoint" || store.tagUID != "tag-uid" {
		t.Fatalf("tag does not retain recorded snapshot: %+v", tags.created)
	}
}

func TestCreateCleansTagBeforeFailing(t *testing.T) {
	for _, test := range []struct {
		name                   string
		snapshotURIAfterCreate string
		actorUIDAfterCreate    string
	}{
		{name: "snapshot changed", snapshotURIAfterCreate: "s3://snapshots/snapshot-2"},
		{name: "actor replaced", actorUIDAfterCreate: "other-actor-uid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &testStore{}
			tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1", snapshotURIAfterCreate: test.snapshotURIAfterCreate, actorUIDAfterCreate: test.actorUIDAfterCreate}
			service := NewService(store, testAuthorizer{}, tags, nil)
			ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})

			_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
			require.Error(t, err)
			require.Equal(t, 1, tags.deleteCalls)
			require.NotEmpty(t, store.failed)
		})
	}
}

func TestDeleteHidesCheckpointBeforeDeletingTag(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1"}, tagUID: "tag-uid"}
	tags := &testTags{created: &ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.GetId()), Uid: "tag-uid"},
		Status:   &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshots/snapshot-1", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}},
	}}
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(context.Background(), testSession{userID: "alice"})

	store.snapshotErr = fmt.Errorf("deletion must use the snapshot returned by BeginDelete")
	tags.getErr = fmt.Errorf("tag lookup unavailable")
	if err := service.Delete(ctx, checkpoint.GetId()); err == nil {
		t.Fatal("Delete() succeeded without verifying the snapshot tag")
	}
	if checkpoint.State != apiv1alpha1.CheckpointState_CHECKPOINT_STATE_DELETING || tags.deleteCalls != 0 || store.deleted {
		t.Fatal("tag lookup failure must leave deletion pending without deleting the tag")
	}
	tags.getErr = nil
	if err := service.Delete(ctx, checkpoint.GetId()); err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != apiv1alpha1.CheckpointState_CHECKPOINT_STATE_DELETING || tags.deleteCalls != 1 || !store.deleted {
		t.Fatalf("checkpoint state = %s, tag deletes = %d, row deleted = %v", checkpoint.State, tags.deleteCalls, store.deleted)
	}
}

func TestDeleteFailedCheckpointSkipsTagDeletion(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_FAILED}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1"}}
	tags := &testTags{} // created is nil: a FAILED checkpoint never retained a tag.
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(context.Background(), testSession{userID: "alice"})

	require.NoError(t, service.Delete(ctx, checkpoint.GetId()))
	require.True(t, store.deleted)
	require.Zero(t, tags.deleteCalls, "a checkpoint that never retained a tag must not attempt to delete one")
}

// TestDeletePinnedByForkIsFailedPrecondition confirms a checkpoint a fork
// pins reports FailedPrecondition, distinct from the NotFound a missing or
// unowned checkpoint reports.
func TestDeletePinnedByForkIsFailedPrecondition(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &testStore{prepared: checkpoint, beginDeleteErr: database.ErrFailedPrecondition}
	service := NewService(store, testAuthorizer{}, &testTags{}, nil)
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})

	err := service.Delete(ctx, checkpoint.GetId())
	require.Equal(t, serviceerrors.CodeFailedPrecondition, serviceerrors.CodeOf(err))
	require.False(t, store.deleted)
}

func TestDeleteRejectsUnownedTagWhenNoTagWasEverRetained(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_FAILED}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1"}}
	// A tag happens to exist at this name with matching snapshot fields but, like
	// the checkpoint's own record, no UID of its own: it is not the one this
	// checkpoint retained, so it must not be deleted on the checkpoint's behalf.
	tags := &testTags{created: &ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.GetId())},
		Status:   &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshots/snapshot-1", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}},
	}}
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(context.Background(), testSession{userID: "alice"})

	err := service.Delete(ctx, checkpoint.GetId())
	require.Error(t, err)
	require.Zero(t, tags.deleteCalls, "an unowned tag must never be deleted")
	require.False(t, store.deleted)
}

// A Full onQuiesce policy records FULL boundaries. They can be checkpointed but
// not forked, because a fork restores only durable data.
func TestFullSnapshotCheckpointsButCannotFork(t *testing.T) {
	store := &testStore{scope: "FULL"}
	tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1", scope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL}
	workflow := &testWorkflow{}
	service := NewService(store, testAuthorizer{}, tags, workflow)
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})

	checkpoint, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, checkpoint.State)
	require.Equal(t, "FULL", store.snapshot.ContentScope)

	_, err = service.Fork(ctx, checkpoint.Id, "fork-request", false)
	require.Equal(t, serviceerrors.CodeFailedPrecondition, serviceerrors.CodeOf(err))
	require.Nil(t, workflow.session, "a FULL checkpoint must not reach runtime creation")

	// The recorded scope still binds the copy: a DATA Actor cannot satisfy a FULL boundary.
	store, tags = &testStore{scope: "FULL"}, &testTags{snapshotURI: "s3://snapshots/snapshot-1"}
	_, err = NewService(store, testAuthorizer{}, tags, nil).Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-2", "task-1")
	require.Error(t, err)
	require.Zero(t, tags.createCalls)
}

func TestForkCreatesSessionFromCheckpoint(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: "DATA"}}
	workflow := &testWorkflow{}
	store.tagUID = "tag-uid"
	tags := &testTags{created: &ateapipb.Tag{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.Id), Uid: store.tagUID}, Status: &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: store.snapshot.URI, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}}}, actorErr: errors.New("source Actor was deleted")}
	service := NewService(store, testAuthorizer{}, tags, workflow)
	ctx := auth.AuthSessionTo(context.Background(), testSession{userID: "alice"})

	session, err := service.Fork(ctx, checkpoint.GetId(), "fork-request", false)
	if err != nil {
		t.Fatal(err)
	}
	if session.GetState() != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY ||
		workflow.session != store.forked || store.forked.GetId() == "" {
		t.Fatalf("fork = %+v, checkpoint = %+v", session, checkpoint)
	}
}

// TestForkPassesCredentialsToStore confirms Fork forwards exactly the
// credentials it was given to the store, and that an invalid reference is
// rejected before it ever reaches the store.
func TestForkPassesCredentialsToStore(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: "DATA"}, tagUID: "tag-uid"}
	tags := &testTags{created: &ateapipb.Tag{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.Id), Uid: store.tagUID}, Status: &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: store.snapshot.URI, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}}}}
	service := NewService(store, testAuthorizer{}, tags, &testWorkflow{})
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
	credentials := []*apiv1alpha1.SessionCredential{{Origin: "https://api.example.com", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: "one"}}}

	session, err := service.Fork(ctx, checkpoint.GetId(), "fork-request", false, credentials...)
	require.NoError(t, err)
	require.Len(t, store.forkCredentials, 1)
	require.True(t, proto.Equal(credentials[0], store.forkCredentials[0]))
	require.True(t, proto.Equal(credentials[0], session.GetCredentials()[0]))

	store.forked, store.forkCredentials = nil, nil
	invalid := []*apiv1alpha1.SessionCredential{{Origin: "not a URL", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: "one"}}}
	_, err = service.Fork(ctx, checkpoint.GetId(), "fork-request-2", false, invalid...)
	require.Equal(t, serviceerrors.CodeInvalidArgument, serviceerrors.CodeOf(err))
	require.Nil(t, store.forkCredentials, "an invalid reference must never reach the store")
}

// TestForkCredentialErrorsMapToServiceCodes confirms the store's
// credential-specific errors surface with the same codes CreateSession uses.
func TestForkCredentialErrorsMapToServiceCodes(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc", State: apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY}
	store := &testStore{prepared: checkpoint, snapshot: &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshots/snapshot-1", ContentScope: "DATA"}, tagUID: "tag-uid"}
	tags := &testTags{created: &ateapipb.Tag{Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: tagName(checkpoint.Id), Uid: store.tagUID}, Status: &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: store.snapshot.URI, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}}}}
	store.forkErr = fmt.Errorf("origin not allowed by revision: %w", database.ErrSessionCredentialNotAllowed)
	service := NewService(store, testAuthorizer{}, tags, &testWorkflow{})
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})

	_, err := service.Fork(ctx, checkpoint.GetId(), "fork-request", false)
	require.Equal(t, serviceerrors.CodeInvalidArgument, serviceerrors.CodeOf(err))
}

func TestCreateRetriesRetainedTagAfterPublishFailure(t *testing.T) {
	store := &testStore{finalizeErr: errors.New("database unavailable")}
	tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1", createErr: status.Error(codes.DeadlineExceeded, "response lost")}
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
	_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
	require.Error(t, err)
	require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_CREATING, store.prepared.State)
	require.Equal(t, 0, tags.deleteCalls)
	retained := tags.created
	store.finalizeErr = nil
	checkpoint, err := service.Create(ctx, store.prepared.SessionId, "request-1", "task-1")
	require.NoError(t, err)
	require.Same(t, retained, tags.created)
	require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, checkpoint.State)
	require.Equal(t, "s3://tags/checkpoint", store.snapshot.URI)
}

func TestCreateRejectsInvalidTagAndKeepsCleanupRetryable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ateapipb.Tag)
	}{
		{"incomplete copy", func(tag *ateapipb.Tag) { tag.Status.Snapshot = nil }},
		{"wrong source", func(tag *ateapipb.Tag) { tag.SourceActor.Name = "other" }},
		{"wrong template", func(tag *ateapipb.Tag) { tag.Status.ActorTemplateUid = "other" }},
		{"missing template", func(tag *ateapipb.Tag) { tag.Status.ActorTemplateUid = "" }},
		{"wrong scope", func(tag *ateapipb.Tag) {
			tag.Status.Snapshot.ContentScope = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &testStore{}
			tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1", mutateTag: tc.mutate, deleteErr: errors.New("cleanup unavailable")}
			service := NewService(store, testAuthorizer{}, tags, nil)
			ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
			_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
			require.Error(t, err)
			require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_CREATING, store.prepared.State)
			tags.deleteErr = nil
			_, err = service.Create(ctx, store.prepared.SessionId, "request-1", "task-1")
			require.Error(t, err)
			require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_FAILED, store.prepared.State)
			require.Nil(t, tags.created)
		})
	}
}

func TestForkRejectsReplacedTag(t *testing.T) {
	checkpoint := &apiv1alpha1.Checkpoint{Id: "018f47a2-4efb-7c21-a848-123456789abc"}
	snapshot := &database.SessionTaskSnapshot{Atespace: "team-a", URI: "s3://tags/checkpoint", ContentScope: "DATA"}
	for _, tc := range []struct{ name, uid, uri string }{
		{"recreated tag", "other-uid", snapshot.URI},
		{"different snapshot", "tag-uid", "s3://tags/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &testStore{prepared: checkpoint, snapshot: snapshot, tagUID: "tag-uid"}
			tags := &testTags{created: &ateapipb.Tag{
				Metadata: &ateapipb.ResourceMetadata{Atespace: snapshot.Atespace, Name: tagName(checkpoint.Id), Uid: tc.uid},
				Status:   &ateapipb.TagStatus{Snapshot: &ateapipb.ExternalSnapshot{SnapshotUri: tc.uri, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}},
			}}
			service := NewService(store, testAuthorizer{}, tags, &testWorkflow{})
			ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
			_, err := service.Fork(ctx, checkpoint.Id, "fork-request", false)
			require.Error(t, err)
			require.Nil(t, store.forked)
		})
	}
}

func TestConcurrentCreateRetainsOneTag(t *testing.T) {
	store := &testStore{}
	tags := &testTags{snapshotURI: "s3://snapshots/snapshot-1", mutateTag: func(*ateapipb.Tag) { runtime.Gosched() }}
	service := NewService(store, testAuthorizer{}, tags, nil)
	ctx := auth.AuthSessionTo(t.Context(), testSession{userID: "alice"})
	start := make(chan struct{})
	results := make(chan error, 32)
	for range cap(results) {
		go func() {
			<-start
			_, err := service.Create(ctx, "018f47a2-4efb-7c21-a848-123456789abc", "request-1", "task-1")
			results <- err
		}()
	}
	close(start)
	for range cap(results) {
		require.NoError(t, <-results)
	}
	require.Equal(t, 1, tags.createCalls)
	require.Equal(t, 0, tags.deleteCalls)
	require.Equal(t, "s3://tags/checkpoint", store.snapshot.URI)
}

func (s *testStore) GetRuntimeGeneration(_ context.Context, id string) (*database.RuntimeGeneration, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return &database.RuntimeGeneration{SessionID: parsed, Atespace: "team-a", ActorName: "session-" + id + "-0123456789abcdef", ActorUID: "actor-uid", Phase: "active"}, nil
}
