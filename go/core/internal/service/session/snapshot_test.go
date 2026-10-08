package session

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestQuiesceSnapshotMatchesPreparedPolicy(t *testing.T) {
	data, full := ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	for _, test := range []struct {
		name    string
		policy  ateapipb.SnapshotContentScope
		actual  ateapipb.SnapshotContentScope
		wantErr bool
	}{
		{name: "Data policy with DATA", policy: data, actual: data},
		{name: "Full policy with FULL", policy: full, actual: full},
		{name: "Data policy rejects FULL", policy: data, actual: full, wantErr: true},
		{name: "Full policy rejects DATA", policy: full, actual: data, wantErr: true},
		{name: "unspecified snapshot rejected", policy: data, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			revision := &database.RuntimeRevision{ActorTemplateAtespace: "team-a", ActorTemplateName: "pinned-template", ActorTemplateUID: "template-uid"}
			template := &ateapipb.ActorTemplate{
				Metadata:       &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "pinned-template", Uid: "template-uid"},
				SnapshotConfig: &ateapipb.SnapshotConfig{OnCommit: test.policy, OnPause: full},
			}
			expected, err := quiesceSnapshotScope(revision, template)
			require.NoError(t, err)
			actor := &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor"},
				Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
					ExternalSnapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshot", ContentScope: test.actual, ActorTemplateUid: "template-uid"}},
			}
			snapshot, err := snapshotForQuiesce(revision, actor, expected)
			if test.wantErr {
				require.Error(t, err)
				require.Nil(t, snapshot)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "team-a", snapshot.Atespace)
			require.Equal(t, "s3://snapshot", snapshot.URI)
			require.Contains(t, []string{"DATA", "FULL"}, snapshot.ContentScope)
		})
	}
}

type capturedSuspendTestActors struct {
	*lifecycleTestActors
	afterCapture func(*ateapipb.Actor, *ateapipb.ActorTemplate)
	loseReply    bool
	captures     int
}

func (a *capturedSuspendTestActors) SuspendActor(ctx context.Context, space, name string) (*ateapipb.Actor, error) {
	before, err := a.GetActor(ctx, space, name)
	if err != nil {
		return nil, err
	}
	if _, err := a.lifecycleTestActors.SuspendActor(ctx, space, name); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	firstCapture := before.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED && a.captures == 0
	if before.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		a.captures++
	}
	if firstCapture && a.afterCapture != nil {
		a.afterCapture(a.actors[actorKey(space, name)], a.template)
	}
	if firstCapture && a.loseReply {
		return nil, status.Error(codes.Unavailable, "response lost after captured suspension")
	}
	return proto.CloneOf(a.actors[actorKey(space, name)]), nil
}

func TestActorWorkflowCapturedSuspendRetriesRemainFenced(t *testing.T) {
	data, full := ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	for _, test := range []struct {
		name           string
		policy, actual ateapipb.SnapshotContentScope
		afterCapture   func(*ateapipb.Actor, *ateapipb.ActorTemplate)
		loseReply      bool
		invalid        bool
	}{
		{name: "DATA rejects FULL across retries", policy: data, actual: full, invalid: true},
		{name: "FULL rejects DATA across retries", policy: full, actual: data, invalid: true},
		{name: "lost invalid FULL reply stays fenced", policy: full, actual: data, loseReply: true, invalid: true},
		{name: "missing URI stays fenced", policy: data, actual: data, invalid: true, afterCapture: func(a *ateapipb.Actor, _ *ateapipb.ActorTemplate) { a.Status.ExternalSnapshot.SnapshotUri = "" }},
		{name: "foreign snapshot template stays fenced", policy: data, actual: data, invalid: true, afterCapture: func(a *ateapipb.Actor, _ *ateapipb.ActorTemplate) {
			a.Status.ExternalSnapshot.ActorTemplateUid = "foreign-template"
		}},
		{name: "replaced pinned template stays fenced", policy: data, actual: data, invalid: true, afterCapture: func(_ *ateapipb.Actor, p *ateapipb.ActorTemplate) { p.Metadata.Uid = "replacement-template" }},
		{name: "unsupported policy stays fenced", policy: data, actual: data, invalid: true, afterCapture: func(_ *ateapipb.Actor, p *ateapipb.ActorTemplate) {
			p.SnapshotConfig.OnCommit = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_UNSPECIFIED
		}},
		{name: "replaced Actor stays fenced", policy: data, actual: data, invalid: true, afterCapture: func(a *ateapipb.Actor, _ *ateapipb.ActorTemplate) { a.Metadata.Uid = "replacement-actor" }},
		{name: "valid DATA lost reply reconciles", policy: data, actual: data, loseReply: true},
		{name: "valid FULL lost reply reconciles", policy: full, actual: full, loseReply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}, snapshotScope: test.actual}
			template, err := base.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
			require.NoError(t, err)
			template.SnapshotConfig.OnCommit = test.policy
			base.template = template
			actors := &capturedSuspendTestActors{lifecycleTestActors: base, afterCapture: test.afterCapture, loseReply: test.loseReply}
			workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			session, err = workflow.Create(t.Context(), session)
			require.NoError(t, err)
			generation, err := store.GetRuntimeGeneration(t.Context(), session.Id)
			require.NoError(t, err)
			_, err = base.ResumeActor(t.Context(), generation.Atespace, generation.ActorName)
			require.NoError(t, err)
			_, err = workflow.Suspend(t.Context(), session)
			require.Error(t, err)
			held, err := store.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
			require.NoError(t, err)
			require.NotEqual(t, uuid.Nil, held.ExecutorID, "the durable operation remembers possibly issued capture")
			captured, err := base.GetActor(t.Context(), generation.Atespace, generation.ActorName)
			require.NoError(t, err)
			for range 2 {
				retry := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
				_, retryErr := retry.Suspend(t.Context(), held.Instance)
				persisted, err := store.GetSessionByID(t.Context(), session.Id)
				require.NoError(t, err)
				after, err := base.GetActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				require.True(t, proto.Equal(captured, after), "retry must not replace or repair rejected state implicitly")
				if test.invalid {
					require.Error(t, retryErr)
					require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, persisted.State)
					require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, persisted.Operation)
					current, err := store.GetSessionOperation(t.Context(), session.Id, held.ID)
					require.NoError(t, err)
					require.NotEqual(t, uuid.Nil, current.ExecutorID)
					_, err = retry.Resume(t.Context(), persisted)
					require.ErrorIs(t, err, database.ErrConflict)
					_, err = retry.Delete(t.Context(), persisted)
					require.ErrorIs(t, err, database.ErrConflict)
				} else {
					require.NoError(t, retryErr)
					require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, persisted.State)
					require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, persisted.Operation)
				}
			}
			require.Equal(t, 1, actors.captures, "a retry must not capture again")
			retained, err := store.GetRuntimeGeneration(t.Context(), session.Id)
			require.NoError(t, err)
			require.Equal(t, generation.ID, retained.ID)
			require.Equal(t, generation.ActorUID, retained.ActorUID)
		})
	}
}

type observedSuspendTestStore struct {
	*lifecycleTestStore
	loseReply bool
	failOnce  bool
	finishes  int
}

func (s *observedSuspendTestStore) FinishObservedSessionSuspension(ctx context.Context, sessionID string, id uuid.UUID, revision, actorUID string) (*apiv1alpha1.Session, error) {
	s.finishes++
	if s.failOnce && s.finishes == 1 && !s.loseReply {
		return nil, status.Error(codes.Unavailable, "database completion failed before commit")
	}
	result, err := s.Client.FinishObservedSessionSuspension(ctx, sessionID, id, revision, actorUID)
	if err == nil && s.failOnce && s.finishes == 1 && s.loseReply {
		return nil, status.Error(codes.Unavailable, "database completion reply lost after commit")
	}
	return result, err
}

type observedSuspendTestActors struct {
	*lifecycleTestActors
	suspends int
}

func (a *observedSuspendTestActors) SuspendActor(context.Context, string, string) (*ateapipb.Actor, error) {
	a.suspends++
	return nil, status.Error(codes.Internal, "observed suspension must not issue a runtime mutation")
}

func TestActorWorkflowInitialSuspendCompletionRetries(t *testing.T) {
	data, full := ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	for _, test := range []struct {
		name   string
		policy ateapipb.SnapshotContentScope
		golden bool
	}{
		{name: "DATA without snapshot", policy: data},
		{name: "FULL without snapshot", policy: full},
		{name: "DATA with borrowed golden FULL", policy: data, golden: true},
	} {
		for _, loseReply := range []bool{false, true} {
			failure := "before commit"
			if loseReply {
				failure = "lost completion reply"
			}
			t.Run(test.name+"/"+failure, func(t *testing.T) {
				baseStore, session := lifecycleFixture(t)
				store := &observedSuspendTestStore{lifecycleTestStore: baseStore, failOnce: true, loseReply: loseReply}
				baseActors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
				template, err := baseActors.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
				require.NoError(t, err)
				template.SnapshotConfig.OnCommit = test.policy
				baseActors.template = template
				actors := &observedSuspendTestActors{lifecycleTestActors: baseActors}
				workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
				session, err = workflow.Create(t.Context(), session)
				require.NoError(t, err)
				generation, err := store.GetRuntimeGeneration(t.Context(), session.Id)
				require.NoError(t, err)
				if test.golden {
					baseActors.mu.Lock()
					baseActors.actors[actorKey(generation.Atespace, generation.ActorName)].Status.ExternalSnapshot = &ateapipb.ExternalSnapshot{
						SnapshotUri: "s3://golden/template", ContentScope: full, ActorTemplateUid: store.revision.ActorTemplateUID,
					}
					baseActors.mu.Unlock()
				}
				initial, err := actors.GetActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				_, err = workflow.Suspend(t.Context(), session)
				require.Error(t, err)
				held, err := store.BeginSessionOperation(t.Context(), session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
				require.NoError(t, err)
				require.Equal(t, uuid.Nil, held.ExecutorID, "failed no-op persistence must not imply issued capture")
				if loseReply {
					require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, held.Instance.Operation)
				} else {
					require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, held.Instance.State)
					require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, held.Instance.Operation)
				}
				retry := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
				session, err = retry.Suspend(t.Context(), held.Instance)
				require.NoError(t, err)
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, session.State)
				require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, session.Operation)
				after, err := actors.GetActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				require.True(t, proto.Equal(initial, after))
				require.Zero(t, actors.suspends, "both attempts settle using only Actor observations")
				_, err = retry.Quiesce(t.Context(), session)
				require.Error(t, err, "terminal task boundaries still require a valid captured snapshot")
				continuation := NewActorWorkflow(store, baseActors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
				session, err = continuation.Resume(t.Context(), session)
				require.NoError(t, err)
				_, err = continuation.Delete(t.Context(), session)
				require.NoError(t, err)
			})
		}
	}
}

func TestQuiesceSnapshotRejectsUntrustedBoundary(t *testing.T) {
	for _, name := range []string{"template UID", "template atespace", "template name", "unsupported policy", "missing policy", "snapshot template UID", "missing snapshot URI", "unsettled Actor"} {
		t.Run(name, func(t *testing.T) {
			revision := &database.RuntimeRevision{ActorTemplateAtespace: "team-a", ActorTemplateName: "pinned-template", ActorTemplateUID: "template-uid"}
			template := &ateapipb.ActorTemplate{
				Metadata:       &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "pinned-template", Uid: "template-uid"},
				SnapshotConfig: &ateapipb.SnapshotConfig{OnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL},
			}
			actor := &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				ExternalSnapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "s3://snapshot", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, ActorTemplateUid: "template-uid"}}}
			switch name {
			case "template UID":
				template.Metadata.Uid = "replacement"
			case "template atespace":
				template.Metadata.Atespace = "other"
			case "template name":
				template.Metadata.Name = "latest"
			case "unsupported policy":
				template.SnapshotConfig.OnCommit = ateapipb.SnapshotContentScope(99)
			case "missing policy":
				template.SnapshotConfig = nil
			case "snapshot template UID":
				actor.Status.ExternalSnapshot.ActorTemplateUid = "replacement"
			case "missing snapshot URI":
				actor.Status.ExternalSnapshot.SnapshotUri = ""
			case "unsettled Actor":
				actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDING
			}
			expected, err := quiesceSnapshotScope(revision, template)
			if err == nil {
				_, err = snapshotForQuiesce(revision, actor, expected)
			}
			require.Error(t, err)
		})
	}
}

func TestActorWorkflowConfiguredSnapshotScope(t *testing.T) {
	data, full := ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	for _, policy := range []ateapipb.SnapshotContentScope{data, full} {
		for _, actual := range []ateapipb.SnapshotContentScope{data, full} {
			for _, operation := range []string{"quiesce", "suspend"} {
				t.Run(policy.String()+"/"+actual.String()+"/"+operation, func(t *testing.T) {
					store, session := lifecycleFixture(t)
					base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}, snapshotScope: actual}
					template, err := base.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
					require.NoError(t, err)
					base.template = template
					base.template.SnapshotConfig.OnCommit = policy
					workflow := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
					session, err = workflow.Create(t.Context(), session)
					require.NoError(t, err)
					// Dispatch starts compute while the Session is already READY.
					_, err = base.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
					require.NoError(t, err)
					if operation == "quiesce" {
						_, err = workflow.Quiesce(t.Context(), session)
					} else {
						session, err = workflow.Suspend(t.Context(), session)
					}
					if policy != actual {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					_, err = workflow.Resume(t.Context(), session)
					require.NoError(t, err, "same Actor resume must support either snapshot scope")
				})
			}
		}
	}
}

func TestPauseAlwaysPreservesFullWaitingTask(t *testing.T) {
	for _, policy := range []ateapipb.SnapshotContentScope{ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL} {
		t.Run(policy.String(), func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			template, err := base.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
			require.NoError(t, err)
			base.template = template
			base.template.SnapshotConfig.OnCommit = policy
			workflow := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			session, err = workflow.Create(t.Context(), session)
			require.NoError(t, err)
			_, err = base.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
			require.NoError(t, err)
			require.NoError(t, workflow.Pause(t.Context(), session))
			actor, err := base.GetActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
			require.NoError(t, err)
			require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, actor.GetStatus().GetLocalSnapshot().GetContentScope())
		})
	}
}

func TestActorWorkflowSuspendBeforeFirstResume(t *testing.T) {
	data, full := ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	for _, test := range []struct {
		name   string
		policy ateapipb.SnapshotContentScope
		golden bool
	}{
		{name: "Data without snapshot", policy: data},
		{name: "Full without snapshot", policy: full},
		{name: "Data with borrowed golden FULL", policy: data, golden: true},
	} {
		for _, continuation := range []string{"resume then delete", "delete"} {
			t.Run(test.name+"/"+continuation, func(t *testing.T) {
				store, session := lifecycleFixture(t)
				actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
				template, err := actors.GetActorTemplate(t.Context(), store.revision.ActorTemplateAtespace, store.revision.ActorTemplateName)
				require.NoError(t, err)
				template.SnapshotConfig.OnCommit = test.policy
				actors.template = template
				workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
				session, err = workflow.Create(t.Context(), session)
				require.NoError(t, err)
				generation, err := store.GetRuntimeGeneration(t.Context(), session.Id)
				require.NoError(t, err)
				if test.golden {
					actors.mu.Lock()
					actors.actors[actorKey(generation.Atespace, generation.ActorName)].Status.ExternalSnapshot = &ateapipb.ExternalSnapshot{
						SnapshotUri: "s3://golden/template", ContentScope: full, ActorTemplateUid: store.revision.ActorTemplateUID,
					}
					actors.mu.Unlock()
				}
				initial, err := actors.GetActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				unchanged, err := actors.SuspendActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				require.True(t, proto.Equal(initial, unchanged), "Substrate's suspended fast path must not manufacture a snapshot")
				_, err = workflow.Quiesce(t.Context(), session)
				require.Error(t, err, "a task boundary must still reject missing state or mismatched golden scope")
				session, err = workflow.Suspend(t.Context(), session)
				require.NoError(t, err)
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, session.State)
				require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, session.Operation)
				persisted, err := store.GetSessionByID(t.Context(), session.Id)
				require.NoError(t, err)
				require.Equal(t, session.State, persisted.State)
				require.Equal(t, session.Operation, persisted.Operation)
				_, err = workflow.Suspend(t.Context(), session)
				require.NoError(t, err, "Suspend retry must remain settled")
				unchanged, err = actors.GetActor(t.Context(), generation.Atespace, generation.ActorName)
				require.NoError(t, err)
				require.True(t, proto.Equal(initial, unchanged), "initial lifecycle suspension retains the existing state")
				if continuation == "resume then delete" {
					session, err = workflow.Resume(t.Context(), session)
					require.NoError(t, err)
					require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, session.State)
					require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, session.Operation)
					resumed, err := store.GetRuntimeGeneration(t.Context(), session.Id)
					require.NoError(t, err)
					require.Equal(t, generation.ID, resumed.ID)
					require.Equal(t, generation.ActorUID, resumed.ActorUID)
					require.Len(t, actors.actors, 1, "Resume must retain the same Actor")
				}
				session, err = workflow.Delete(t.Context(), session)
				require.NoError(t, err)
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, session.State)
				require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, session.Operation)
				require.Empty(t, actors.actors)
			})
		}
	}
}
