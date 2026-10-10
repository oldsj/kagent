package grpcserver

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Runtime creation is tested by the Session workflow suite. This test exercises
// transport, validation, authentication, idempotency and real persistence.
type credentialTestWorkflow struct{ *sessionsvc.ActorWorkflow }

func (credentialTestWorkflow) Create(_ context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	return session, nil
}

func TestSessionCredentialsThroughGRPC(t *testing.T) {
	dsn := dbtest.StartT(context.WithoutCancel(t.Context()), t)
	dbtest.MigrateT(t, dsn, false)
	db, err := database.Connect(t.Context(), &database.PostgresConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	store := database.NewClient(db)
	definition := database.AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "agent-uid", DesiredRevision: "credential-revision"}
	require.NoError(t, store.UpsertAgentDefinition(t.Context(), definition))
	require.NoError(t, store.RecordRuntimeRevision(t.Context(), database.RuntimeRevision{Revision: definition.DesiredRevision, Namespace: definition.Namespace, AgentName: definition.AgentName, AgentUID: definition.AgentUID, SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, EgressDestinations: []string{"http://echo.test.svc.cluster.local:80"}, ActorTemplateAtespace: "team-a", ActorTemplateName: "credential-template", ActorTemplateUID: "template-uid"}, true))
	listener := bufconn.Listen(DefaultMaxMessageSize)
	server, err := New(Config{Listener: listener, Authenticator: &authimpl.InsecureAuthenticator{}, SystemService: testSystemService(), SessionService: sessionsvc.NewService(store, &auth.NoopAuthorizer{}, credentialTestWorkflow{})})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	conn, err := grpc.NewClient("passthrough:///credential-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := apiv1alpha1.NewSessionServiceClient(conn)
	owner := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-user-id", "alice"))
	request := &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "credential-create", Credentials: []*apiv1alpha1.SessionCredential{{Origin: "http://echo.test.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: "binding-one"}}}}
	created, err := client.CreateSession(owner, request)
	require.NoError(t, err)
	get, err := client.GetSession(owner, &apiv1alpha1.GetSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	require.True(t, proto.Equal(request.Credentials[0], get.Session.Credentials[0]))
	retry, err := client.CreateSession(owner, request)
	require.NoError(t, err)
	require.Equal(t, created.Session.Id, retry.Session.Id)
	changed := proto.CloneOf(request)
	changed.Credentials[0].SecretRef.Key = "binding-two"
	_, err = client.CreateSession(owner, changed)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	changed.RequestId = "unlisted"
	changed.Credentials[0].Origin = "http://unlisted.test.svc.cluster.local"
	_, err = client.CreateSession(owner, changed)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	changed.RequestId = "path"
	changed.Credentials[0].Origin = "http://echo.test.svc.cluster.local/mcp"
	_, err = client.CreateSession(owner, changed)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

type proxyControlledActors struct {
	mu                         sync.Mutex
	actor                      *ateapipb.Actor
	templateUID                string
	policy                     *ateapipb.EgressPolicy
	creates, resumes, suspends int
}

func (a *proxyControlledActors) GetActor(context.Context, string, string) (*ateapipb.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.actor == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	return proto.CloneOf(a.actor), nil
}

func (a *proxyControlledActors) GetActorTemplate(_ context.Context, atespace, name string) (*ateapipb.ActorTemplate, error) {
	return &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: a.templateUID},
		SnapshotConfig: &ateapipb.SnapshotConfig{
			OnPause:  ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
		},
	}, nil
}

func (a *proxyControlledActors) CreateActor(_ context.Context, at, name, templateAt, templateName string) (*ateapipb.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creates++
	a.actor = &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: at, Name: name, Uid: "controlled-actor-uid"}, ActorTemplate: &ateapipb.ObjectRef{Atespace: templateAt, Name: templateName}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}}
	return proto.CloneOf(a.actor), nil
}
func (a *proxyControlledActors) CreateActorFromTag(context.Context, string, string, string, string, string, string) (*ateapipb.Actor, error) {
	return nil, status.Error(codes.Unimplemented, "no fork fixture")
}
func (a *proxyControlledActors) EnsureActorEgressPolicy(_ context.Context, _, _ string, p *ateapipb.EgressPolicy) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.policy = proto.CloneOf(p)
	return nil
}
func (a *proxyControlledActors) ResumeActor(context.Context, string, string) (*ateapipb.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumes++
	a.actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	return proto.CloneOf(a.actor), nil
}
func (a *proxyControlledActors) SuspendActor(context.Context, string, string) (*ateapipb.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.suspends++
	a.actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	a.actor.Status.ExternalSnapshot = &ateapipb.ExternalSnapshot{
		SnapshotUri: "s3://snapshots/controlled-actor", ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, ActorTemplateUid: a.templateUID,
	}
	return proto.CloneOf(a.actor), nil
}
func (a *proxyControlledActors) PauseActor(context.Context, string, string) (*ateapipb.Actor, error) {
	return nil, status.Error(codes.Unimplemented, "no turn fixture")
}
func (a *proxyControlledActors) DeleteActor(context.Context, string, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actor = nil
	return nil
}

func TestGitProxyReferenceOnlyGRPCReadiness(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.StartT(context.WithoutCancel(ctx), t)
	dbtest.MigrateT(t, dsn, false)
	db, err := database.Connect(ctx, &database.PostgresConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	store := database.NewClient(db)
	revision := database.RuntimeRevision{Revision: "proxy-revision", Namespace: "kagent", AgentName: "mainloop-main", AgentUID: "agent-uid", SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{},
		GitOrigins: []string{"github.com"}, EgressDestinations: []string{workspace.ReadProxyOrigin + ":80", workspace.PushProxyOrigin + ":80", "http://mainloop-mcp.mainloop.svc.cluster.local:80", "https://api.openai.com:443"},
		Credentials:           []egress.Credential{{Hostname: "api.openai.com", Header: "authorization", Prefix: "Bearer ", URI: "ate-secret://k8s.io/default/kagent/model/key"}},
		ActorTemplateAtespace: "kagent", ActorTemplateName: "proxy-template", ActorTemplateUID: "template-uid"}
	require.NoError(t, store.UpsertAgentDefinition(ctx, database.AgentDefinition{Namespace: revision.Namespace, AgentName: revision.AgentName, AgentUID: revision.AgentUID, DesiredRevision: revision.Revision}))
	require.NoError(t, store.RecordRuntimeRevision(ctx, revision, true))
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, CleartextMCPOrigins: []string{"http://mainloop-mcp.mainloop.svc.cluster.local"}, Credentials: []controlauth.CredentialRule{
		{Namespace: "kagent", SecretNamePattern: "mainloop-mcp-binding-*", Key: "authorization", Header: "authorization", Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Purpose: "mcp"},
		{Namespace: "kagent", SecretNamePattern: "mainloop-git-read-*", Key: "authorization", Header: "authorization", Origin: workspace.ReadProxyOrigin, Purpose: "git-read"},
		{Namespace: "kagent", SecretNamePattern: "mainloop-git-push-*", Key: "authorization", Header: "authorization", Origin: workspace.PushProxyOrigin, Purpose: "git-push"},
	}})
	require.NoError(t, err)
	tokenFile := filepath.Join(t.TempDir(), "control-token")
	token := strings.Repeat("fixture", 8)
	require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0600))
	authenticator, err := authimpl.NewServiceTokenAuthenticator(tokenFile, "", policy)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	actors := &proxyControlledActors{templateUID: revision.ActorTemplateUID}
	workflow := sessionsvc.NewActorWorkflow(store, actors, substrate.NewRuntimeCredentialIssuer(kube, "kagent"), "http://kagent-controller.kagent:8083")
	listener := bufconn.Listen(DefaultMaxMessageSize)
	server, err := New(Config{Listener: listener, Authenticator: authenticator, SystemService: testSystemService(), SessionService: sessionsvc.NewService(store, policy, workflow)})
	require.NoError(t, err)
	serverCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Start(serverCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	conn, err := grpc.NewClient("passthrough:///proxy-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := apiv1alpha1.NewSessionServiceClient(conn)
	owner := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	request := &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}, RequestId: "proxy-reference-only", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/owner/repo", Ref: "main", Branch: "feature-case"}, Credentials: []*apiv1alpha1.SessionCredential{
		{Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-mcp-binding-unpublished", Key: "authorization"}},
		{Origin: workspace.ReadProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-read-unpublished", Key: "authorization"}},
		{Origin: workspace.PushProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-push-unpublished", Key: "authorization"}},
	}}
	created, err := client.CreateSession(owner, request)
	require.NoError(t, err)
	id := created.Session.Id
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, created.Session.State)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, actors.actor.Status.State)
	generation, err := store.GetRuntimeGeneration(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "active", generation.Phase)
	get, err := client.GetSession(owner, &apiv1alpha1.GetSessionRequest{SessionId: id})
	require.NoError(t, err)
	require.Nil(t, get.Session.RuntimeAssociation)
	require.True(t, proto.Equal(request.Workspace, get.Session.Workspace))
	frozen := proto.CloneOf(get.Session)
	refs := &corev1.SecretList{}
	require.NoError(t, kube.List(ctx, refs))
	require.Len(t, refs.Items, 1)
	require.True(t, strings.HasPrefix(refs.Items[0].Name, egress.RuntimeSecretPrefix), "only runtime callback issued")
	require.Equal(t, "true", refs.Items[0].Labels["kagent.dev/runtime-injection"])
	require.Empty(t, refs.Items[0].Labels["mainloop.dev/actor-egress"])
	wire, err := proto.Marshal(actors.policy)
	require.NoError(t, err)
	for _, ref := range request.Credentials {
		require.Contains(t, string(wire), "/"+ref.SecretRef.Name+"/authorization")
	}
	require.Contains(t, string(wire), generation.CredentialURI)
	require.NotContains(t, string(wire), string(refs.Items[0].Data["token"]))
	// READY Resume wakes the same suspended Actor in place; the reply itself
	// carries no association, which only a fresh GetSession observes.
	woken, err := client.ResumeSession(owner, &apiv1alpha1.ResumeSessionRequest{SessionId: id})
	require.NoError(t, err)
	require.Nil(t, woken.Session.RuntimeAssociation)
	require.Equal(t, 1, actors.resumes)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, actors.actor.Status.State)
	get, err = client.GetSession(owner, &apiv1alpha1.GetSessionRequest{SessionId: id})
	require.NoError(t, err)
	require.Equal(t, generation.ActorUID, get.Session.GetRuntimeAssociation().GetActorUid())
	require.Equal(t, generation.ID.String(), get.Session.GetRuntimeAssociation().GetGenerationId())
	suspended, err := client.SuspendSession(owner, &apiv1alpha1.SuspendSessionRequest{SessionId: id})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, suspended.Session.State)
	_, err = client.ResumeSession(owner, &apiv1alpha1.ResumeSessionRequest{SessionId: id})
	require.NoError(t, err)
	require.Equal(t, 2, actors.resumes)
	require.Equal(t, ateapipb.ActorState_ACTOR_STATE_RUNNING, actors.actor.Status.State)
	get, err = client.GetSession(owner, &apiv1alpha1.GetSessionRequest{SessionId: id})
	require.NoError(t, err)
	association := get.Session.RuntimeAssociation
	require.NotNil(t, association)
	require.Equal(t, generation.ID.String(), association.GenerationId)
	require.Equal(t, generation.ActorUID, association.ActorUid)
	require.Equal(t, generation.ActorName, association.ActorName)
	require.Equal(t, generation.Atespace, association.Atespace)
	require.Equal(t, "active", association.Phase)
	require.True(t, association.CurrentActive)
	observed := proto.CloneOf(get.Session)
	observed.RuntimeAssociation = nil
	require.False(t, observed.UpdatedAt.AsTime().Before(frozen.UpdatedAt.AsTime()))
	observed.UpdatedAt = frozen.UpdatedAt // Lifecycle updates its observation timestamp.
	require.True(t, proto.Equal(frozen, observed), "warmup cannot change tuple/workspace/revision")
	require.NoError(t, kube.List(ctx, refs))
	require.Len(t, refs.Items, 1, "Git values still unpublished after RUNNING association")
	retry, err := client.CreateSession(owner, request)
	require.NoError(t, err)
	require.Equal(t, id, retry.Session.Id)
	require.Equal(t, 1, actors.creates)
	changed := proto.CloneOf(request)
	changed.Credentials[1].SecretRef.Name = "mainloop-git-read-different"
	_, err = client.CreateSession(owner, changed)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	changed = proto.CloneOf(request)
	changed.RequestId = "swapped"
	changed.Credentials[1].Origin = workspace.PushProxyOrigin
	_, err = client.CreateSession(owner, changed)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	t.Log("reference-only READY/SUSPENDED -> READY Resume wakes same Actor -> Suspend/Resume -> READY/RUNNING exact active association; no Git value or native turn")
}
