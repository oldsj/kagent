package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Unimplemented embedded ports panic if inspection attempts any side effect.
type associationTestStore struct {
	workflowStore
	session         *api.Session
	generation      *database.RuntimeGeneration
	generationReads int
	sessionReads    int
	generationErr   error
	sessionErr      error
	beforeRead      func()
}

func (s *associationTestStore) GetRuntimeGeneration(context.Context, string) (*database.RuntimeGeneration, error) {
	s.generationReads++
	if s.beforeRead != nil {
		s.beforeRead()
	}
	if s.generation == nil {
		return nil, s.generationErr
	}
	copy := *s.generation
	return &copy, s.generationErr
}

func (s *associationTestStore) GetSessionForRuntime(context.Context, string, string) (*api.Session, error) {
	s.sessionReads++
	return proto.CloneOf(s.session), s.sessionErr
}

type associationTestActors struct {
	actorClient
	actor *ateapipb.Actor
	err   error
	calls int
	hook  func()
	space string
	name  string
}

func (a *associationTestActors) GetActor(_ context.Context, space, name string) (*ateapipb.Actor, error) {
	a.calls++
	a.space, a.name = space, name
	if a.hook != nil {
		a.hook()
	}
	return proto.CloneOf(a.actor), a.err
}

func associationUnitFixture() (*associationTestStore, *associationTestActors, *api.Session) {
	session := &api.Session{Id: uuid.NewString(), Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "team-a", Name: "assistant"},
		PreparedRevision: "revision", State: api.RuntimeState_RUNTIME_STATE_READY, Operation: api.RuntimeOperation_RUNTIME_OPERATION_NONE}
	generation := &database.RuntimeGeneration{ID: uuid.New(), SessionID: uuid.MustParse(session.Id), Atespace: "team-a", ActorName: "issued-name", ActorUID: "issued-uid", Phase: "active",
		CredentialURI: "ate-secret://sentinel-private-reference/token", TokenDigest: []byte(strings.Repeat("sentinel-digest", 3))}
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: generation.Atespace, Name: generation.ActorName, Uid: generation.ActorUID},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING, ExternalSnapshot: &ateapipb.ExternalSnapshot{SnapshotUri: "sentinel-private-snapshot"}}}
	return &associationTestStore{session: proto.CloneOf(session), generation: generation}, &associationTestActors{actor: actor}, session
}

func TestRuntimeAssociationExactObservation(t *testing.T) {
	store, actors, session := associationUnitFixture()
	workflow := NewActorWorkflow(store, actors, nil, "")
	serviceStore := &serviceTestStore{getResult: session}
	service := NewService(serviceStore, serviceTestAuthorizer{}, workflow)
	result, err := service.Get(serviceTestContext("mainloop"), session.Id)
	require.NoError(t, err)
	require.Equal(t, &api.RuntimeAssociation{GenerationId: store.generation.ID.String(), Atespace: "team-a", ActorName: "issued-name", ActorUid: "issued-uid", Phase: "active", CurrentActive: true}, result.RuntimeAssociation)
	require.Nil(t, session.RuntimeAssociation, "the store's object must not cache an observation")
	require.Equal(t, 2, store.generationReads)
	require.Equal(t, 2, store.sessionReads)
	require.Equal(t, 1, actors.calls)
	require.Equal(t, store.generation.Atespace, actors.space)
	require.Equal(t, store.generation.ActorName, actors.name)
	assertAssociationResponsePrivate(t, result)
	// A subsequent read must consult the Actor again; the first response remains
	// an observation of its own read, never an input to the next one.
	actors.actor.Metadata.Uid = "replacement"
	result, err = service.Get(serviceTestContext("mainloop"), session.Id)
	require.NoError(t, err)
	require.Nil(t, result.RuntimeAssociation)
	require.Equal(t, 2, actors.calls)
}

func assertAssociationResponsePrivate(t *testing.T, session *api.Session) {
	t.Helper()
	response := &api.GetSessionResponse{Session: session}
	encoded, err := protojson.Marshal(response)
	require.NoError(t, err)
	wire, err := proto.Marshal(response)
	require.NoError(t, err)
	for _, sentinel := range []string{"sentinel-digest", "sentinel-private-reference", "sentinel-private-snapshot", "sentinel-token"} {
		require.NotContains(t, string(encoded), sentinel)
		require.NotContains(t, string(wire), sentinel)
	}
}

func TestRuntimeAssociationRejectsInactiveOrMismatchedObservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*associationTestStore, *associationTestActors, *api.Session)
		lookup bool
	}{
		{"missing generation", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.generation = nil
			s.generationErr = database.ErrNotFound
		}, false},
		{"revoked generation", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.generation.Phase = "revoked"
		}, false},
		{"bound generation", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) { s.generation.Phase = "bound" }, false},
		{"foreign generation", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.generation.SessionID = uuid.New()
		}, false},
		{"empty generation", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) { s.generation.ID = uuid.Nil }, false},
		{"unbound UID", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) { s.generation.ActorUID = "" }, false},
		{"suspended Session", func(_ *associationTestStore, _ *associationTestActors, s *api.Session) {
			s.State = api.RuntimeState_RUNTIME_STATE_SUSPENDED
		}, false},
		{"pending Session", func(_ *associationTestStore, _ *associationTestActors, s *api.Session) {
			s.Operation = api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND
		}, false},
		{"deleted durable Session", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.sessionErr = database.ErrNotFound
		}, false},
		{"foreign durable owner", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) { s.session.Creator = "other" }, false},
		{"foreign durable Agent", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.session.Agent.Name = "other"
		}, false},
		{"changed durable revision", func(s *associationTestStore, _ *associationTestActors, _ *api.Session) {
			s.session.PreparedRevision = "other"
		}, false},
		{"missing Actor", func(_ *associationTestStore, a *associationTestActors, _ *api.Session) {
			a.actor = nil
			a.err = status.Error(codes.NotFound, "missing")
		}, true},
		{"nil Actor", func(_ *associationTestStore, a *associationTestActors, _ *api.Session) { a.actor = nil }, true},
		{"replaced UID", func(_ *associationTestStore, a *associationTestActors, _ *api.Session) {
			a.actor.Metadata.Uid = "foreign"
		}, true},
		{"wrong atespace", func(_ *associationTestStore, a *associationTestActors, _ *api.Session) {
			a.actor.Metadata.Atespace = "foreign"
		}, true},
		{"wrong name", func(_ *associationTestStore, a *associationTestActors, _ *api.Session) {
			a.actor.Metadata.Name = "foreign"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			tc.change(store, actors, session)
			association, err := NewActorWorkflow(store, actors, nil, "").InspectRuntime(t.Context(), session)
			require.NoError(t, err)
			require.Nil(t, association)
			require.Equal(t, tc.lookup, actors.calls > 0)
		})
	}
	for _, phase := range []string{"allocated", "secret-issued", "actor-issued", "unknown", ""} {
		t.Run("generation phase "+phase, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			store.generation.Phase = phase
			association, err := NewActorWorkflow(store, actors, nil, "").InspectRuntime(t.Context(), session)
			require.NoError(t, err)
			require.Nil(t, association)
			require.Zero(t, actors.calls)
		})
	}
	for _, state := range ateapipb.ActorState_name {
		value := ateapipb.ActorState(ateapipb.ActorState_value[state])
		if value == ateapipb.ActorState_ACTOR_STATE_RUNNING {
			continue
		}
		t.Run(state, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			actors.actor.Status.State = value
			association, err := NewActorWorkflow(store, actors, nil, "").InspectRuntime(t.Context(), session)
			require.NoError(t, err)
			require.Nil(t, association)
		})
	}
}

func TestRuntimeAssociationSessionChangesDuringObservation(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*associationTestStore)
	}{
		{"missing", func(s *associationTestStore) { s.sessionErr = database.ErrNotFound }},
		{"suspended", func(s *associationTestStore) { s.session.State = api.RuntimeState_RUNTIME_STATE_SUSPENDED }},
		{"pending", func(s *associationTestStore) { s.session.Operation = api.RuntimeOperation_RUNTIME_OPERATION_DELETE }},
		{"owner", func(s *associationTestStore) { s.session.Creator = "other" }},
		{"Agent", func(s *associationTestStore) { s.session.Agent.Name = "other" }},
		{"namespace", func(s *associationTestStore) { s.session.Agent.Namespace = "other" }},
		{"revision", func(s *associationTestStore) { s.session.PreparedRevision = "other" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			actors.hook = func() { change.apply(store) }
			association, err := NewActorWorkflow(store, actors, nil, "").InspectRuntime(t.Context(), session)
			require.NoError(t, err)
			require.Nil(t, association)
		})
	}
}

func TestRuntimeAssociationReadFailuresNeverReturnPositive(t *testing.T) {
	for _, stage := range []string{"generation before", "Session before", "Actor", "Session after", "generation after"} {
		t.Run(stage, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			failure := errors.New("sentinel-token sentinel-private-reference")
			switch stage {
			case "generation before":
				store.generationErr = failure
			case "Session before":
				store.sessionErr = failure
			case "Actor":
				actors.err = failure
			case "Session after":
				actors.hook = func() { store.sessionErr = failure }
			case "generation after":
				actors.hook = func() { store.generationErr = failure }
			}
			service := NewService(&serviceTestStore{getResult: session}, serviceTestAuthorizer{}, NewActorWorkflow(store, actors, nil, ""))
			result, err := service.Get(serviceTestContext("mainloop"), session.Id)
			require.Nil(t, result)
			require.True(t, serviceerrors.IsCode(err, serviceerrors.CodeUnavailable))
			require.NotContains(t, err.Error(), "sentinel")
		})
	}
}

func TestRuntimeAssociationRechecksEveryDurableIdentityField(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*database.RuntimeGeneration)
	}{
		{"generation ID", func(g *database.RuntimeGeneration) { g.ID = uuid.New() }},
		{"Session ID", func(g *database.RuntimeGeneration) { g.SessionID = uuid.New() }},
		{"atespace", func(g *database.RuntimeGeneration) { g.Atespace = "other" }},
		{"name", func(g *database.RuntimeGeneration) { g.ActorName = "other" }},
		{"UID", func(g *database.RuntimeGeneration) { g.ActorUID = "other" }},
		{"phase", func(g *database.RuntimeGeneration) { g.Phase = "revoked" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			store.beforeRead = func() {
				if store.generationReads == 2 {
					change.apply(store.generation)
				}
			}
			association, err := NewActorWorkflow(store, actors, nil, "").InspectRuntime(t.Context(), session)
			require.NoError(t, err)
			require.Nil(t, association)
		})
	}
}

func TestRuntimeAssociationAuthorizationAndOutputIsolation(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "team-a", Agents: []string{"assistant"}})
	require.NoError(t, err)
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{})
	for _, scenario := range []string{"own", "foreign creator", "foreign Agent", "foreign namespace", "share", "list", "cached revoked", "resume"} {
		t.Run(scenario, func(t *testing.T) {
			store, actors, session := associationUnitFixture()
			cached := &api.RuntimeAssociation{GenerationId: uuid.NewString(), ActorUid: "forged", CurrentActive: true}
			session.RuntimeAssociation = cached
			serviceStore := &serviceTestStore{getResult: session, sessions: []*api.Session{session}}
			service := NewService(serviceStore, policy, NewActorWorkflow(store, actors, nil, ""))
			callCtx := ctx
			switch scenario {
			case "foreign creator":
				session.Creator = "other"
			case "foreign Agent":
				session.Agent.Name = "other"
			case "foreign namespace":
				session.Agent.Namespace = "other"
			case "share":
				callCtx = auth.ShareContextTo(ctx, &auth.ShareContext{SessionID: session.Id, UserID: "mainloop", ReadOnly: true})
			case "cached revoked":
				store.generation.Phase = "revoked"
			case "resume":
				// The lifecycle port receives only a cleaned Session.
				service.workflow = serviceTestWorkflow{}
				result, err := service.Resume(ctx, session.Id)
				require.NoError(t, err)
				require.Nil(t, result.RuntimeAssociation)
				require.Same(t, cached, session.RuntimeAssociation)
				return
			}
			if scenario == "list" {
				page, err := service.List(ctx, ListRequest{})
				require.NoError(t, err)
				require.Len(t, page.Sessions, 1)
				require.Nil(t, page.Sessions[0].RuntimeAssociation)
			} else {
				result, err := service.Get(callCtx, session.Id)
				switch scenario {
				case "foreign creator", "foreign Agent", "foreign namespace":
					require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
					require.Nil(t, result)
				default:
					require.NoError(t, err)
					if scenario == "own" {
						require.True(t, result.GetRuntimeAssociation().GetCurrentActive())
					} else {
						require.Nil(t, result.RuntimeAssociation)
					}
				}
			}
			if scenario != "own" {
				require.Zero(t, actors.calls)
			}
			require.Same(t, cached, session.RuntimeAssociation)
		})
	}
}

func TestRuntimeAssociationCannotBeSubmittedAsLifecycleInput(t *testing.T) {
	for _, request := range []proto.Message{&api.CreateSessionRequest{}, &api.ResumeSessionRequest{}, &api.ForkSessionRequest{}} {
		require.Error(t, protojson.Unmarshal([]byte(`{"runtime_association":{"actor_uid":"forged","current_active":true}}`), request))
		require.Nil(t, request.ProtoReflect().Descriptor().Fields().ByName("runtime_association"))
	}
	// Unknown wire fields survive decoding but cannot enter the typed creation
	// arguments used by the gRPC handler.
	request := &api.CreateSessionRequest{Agent: &api.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "create"}
	wire, err := proto.Marshal(request)
	require.NoError(t, err)
	forged, err := proto.Marshal(&api.RuntimeAssociation{GenerationId: uuid.NewString(), ActorUid: "forged", CurrentActive: true})
	require.NoError(t, err)
	wire = protowire.AppendTag(wire, 20, protowire.BytesType)
	wire = protowire.AppendBytes(wire, forged)
	require.NoError(t, proto.Unmarshal(wire, request))
	require.NotEmpty(t, request.ProtoReflect().GetUnknown())
	serviceStore := &serviceTestStore{}
	service := NewService(serviceStore, serviceTestAuthorizer{}, serviceTestWorkflow{})
	created, err := service.CreateWithEnvironment(serviceTestContext("mainloop"), request.GetAgent(), request.GetRequestId(), request.GetName(), request.GetWorkspace(), request.GetDevelopmentEnvironment(), request.GetCredentials()...)
	require.NoError(t, err)
	require.Nil(t, created.RuntimeAssociation)
	require.Nil(t, serviceStore.createInput.RuntimeAssociation)
}
