package grpcserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/internal/service/taskstore"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	claudeexecutor "github.com/kagent-dev/kagent/go/harness/claude/executor"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	codexexecutor "github.com/kagent-dev/kagent/go/harness/codex/executor"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type preparationActors struct {
	*proxyControlledActors
	template *ateapipb.ActorTemplate
}

func (a *preparationActors) GetActorTemplate(context.Context, string, string) (*ateapipb.ActorTemplate, error) {
	return proto.CloneOf(a.template), nil
}

type fixedEnvironmentPreparer struct {
	revision    string
	composition *api.RuntimeComposition
}

func (f fixedEnvironmentPreparer) Prepare(context.Context, *api.ResourceReference, *api.DevelopmentEnvironment) (string, *api.RuntimeComposition, error) {
	return f.revision, proto.CloneOf(f.composition), nil
}

type preparationRuntimeClient struct {
	client api.TaskStoreServiceClient
	ctx    context.Context
}

func (c preparationRuntimeClient) GetWorkspace(ctx context.Context) (*api.Workspace, error) {
	r, e := c.GetWorkspacePreparation(ctx)
	return r.GetWorkspace(), e
}
func (c preparationRuntimeClient) GetWorkspacePreparation(ctx context.Context) (*api.TaskStoreServiceGetWorkspaceResponse, error) {
	return c.client.GetWorkspace(c.callContext(ctx), &api.TaskStoreServiceGetWorkspaceRequest{SessionId: c.sessionID()})
}
func (c preparationRuntimeClient) CompleteWorkspacePreparation(ctx context.Context, r *api.TaskStoreServiceCompleteWorkspacePreparationRequest) error {
	_, err := c.client.CompleteWorkspacePreparation(c.callContext(ctx), r)
	return err
}

type preparationIdentityKey struct{}

func (c preparationRuntimeClient) sessionID() string {
	return c.ctx.Value(preparationIdentityKey{}).(string)
}
func (c preparationRuntimeClient) callContext(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(c.ctx)
	return metadata.NewOutgoingContext(ctx, md.Copy())
}

func nativeGit(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	require.NoError(t, os.MkdirAll(source, 0755))
	run := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	run(source, "init", "-q", "-b", "main")
	run(source, "config", "user.name", "fixture")
	run(source, "config", "user.email", "fixture@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(source, "tracked"), []byte("original source\n"), 0600))
	run(source, "add", "tracked")
	run(source, "commit", "-qm", "fixture")
	sha := run(source, "rev-parse", "HEAD")
	bare := filepath.Join(root, "repo.git")
	run(root, "clone", "-q", "--bare", source, bare)
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "mainloop-git-read.mainloop.svc.cluster.local" || !strings.HasPrefix(r.URL.Path, "/owner/repo.git/") || strings.Contains(r.URL.RequestURI(), "receive-pack") {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		require.Equal(t, "Basic cGxhY2Vob2xkZXI=", r.Header.Get("Authorization"))
		r.URL.Path = "/repo.git/" + strings.TrimPrefix(r.URL.Path, "/owner/repo.git/")
		r.URL.Host = ""
		r.URL.Scheme = ""
		r.RequestURI = r.URL.RequestURI()
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return sha, []string{"PATH=" + os.Getenv("PATH"), "http_proxy=" + proxy.URL, "HTTP_PROXY=" + proxy.URL, "https_proxy=" + proxy.URL, "HTTPS_PROXY=" + proxy.URL, "ALL_PROXY=" + proxy.URL, "all_proxy=" + proxy.URL, "NO_PROXY=", "no_proxy=", "GIT_CONFIG_COUNT=0"}
}

func TestNativePreparationInstalledGRPCPostgres(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) { nativePreparationInstalled(t, provider, "ordinary", "child") })
	}
}

func TestNativePreparationOwnerWorkspaceGRPCPostgres(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) { nativePreparationInstalled(t, provider, "lifecycle", "agent") })
	}
}

func TestNativePreparationLifecycleContinuationPostgres(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) { nativePreparationInstalled(t, provider, "lifecycle", "child") })
	}
}

func TestNativePreparationClaudeDurableRestartPostgres(t *testing.T) {
	for _, scenario := range []string{"restart", "restart-protected"} {
		t.Run(scenario, func(t *testing.T) { nativePreparationInstalled(t, "claude", scenario, "child") })
	}
}

func TestNativePreparationClaudeProtectedMCPPostgres(t *testing.T) {
	nativePreparationInstalled(t, "claude", "protected", "child")
}

// The first callback is committed but its reply is lost. A fresh executor must
// reconcile the identical durable result, rather than perform setup again.
type lostPreparationReply struct {
	preparationRuntimeClient
	committed chan *api.TaskStoreServiceCompleteWorkspacePreparationRequest
}

func (c lostPreparationReply) CompleteWorkspacePreparation(ctx context.Context, r *api.TaskStoreServiceCompleteWorkspacePreparationRequest) error {
	if err := c.preparationRuntimeClient.CompleteWorkspacePreparation(ctx, r); err != nil {
		return err
	}
	c.committed <- proto.CloneOf(r)
	<-ctx.Done()
	return status.Error(codes.DeadlineExceeded, "fixture lost completion reply")
}

type capturedPreparationReply struct {
	preparationRuntimeClient
	completed chan *api.TaskStoreServiceCompleteWorkspacePreparationRequest
}

func (c capturedPreparationReply) CompleteWorkspacePreparation(ctx context.Context, r *api.TaskStoreServiceCompleteWorkspacePreparationRequest) error {
	err := c.preparationRuntimeClient.CompleteWorkspacePreparation(ctx, r)
	if err == nil {
		select {
		case c.completed <- proto.CloneOf(r):
		default:
		}
	}
	return err
}

func nativePreparationInstalled(t *testing.T, provider, scenario, profile string) {
	t.Helper()
	if testing.Short() {
		t.Skip("real PostgreSQL integration")
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	dsn := dbtest.StartT(context.WithoutCancel(ctx), t)
	dbtest.MigrateT(t, dsn, false)
	pool, err := database.Connect(ctx, &database.PostgresConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := database.NewClient(pool)
	sha, gitEnvironment := nativeGit(t)
	data := t.TempDir()
	nativeLog := filepath.Join(data, "native-calls")
	cli := filepath.Join(data, "fake-native")
	var raw []byte
	var cliVersion string
	policyGit := &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
	if provider == "claude" {
		cfg := claudeconfig.Production("fixture-model", "original instructions")
		cfg.ClaudeExecutable = cli
		cfg.Git = policyGit
		cfg.MCPServers = map[string]claudeconfig.MCPServer{"mainloop": {Type: "http", URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp", RequireApproval: strings.Contains(scenario, "protected")}}
		raw, err = json.Marshal(cfg)
		cliVersion = cfg.ExpectedClaudeVersion
	} else {
		cfg := codexconfig.Production("fixture-model", "original instructions")
		cfg.CodexExecutable = cli
		cfg.Provider = codexconfig.Provider{Name: "openai"}
		cfg.Git = policyGit
		cfg.MCPServers = map[string]codexconfig.MCPServer{"mainloop": {URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}
		raw, err = json.Marshal(cfg)
		cliVersion = cfg.ExpectedCodexVersion
	}
	require.NoError(t, err)
	versionLine := "codex-cli " + cliVersion
	if provider == "claude" {
		versionLine = cliVersion + " (Claude Code)"
	}
	require.NoError(t, os.WriteFile(cli, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '"+versionLine+"'; exit 0; fi\nprintf 'native call\\n' >> '"+nativeLog+"'\nexit 42\n"), 0700))
	environment := &api.DevelopmentEnvironment{Image: "fixture.test/d@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64", PolicyIdentity: "fixture-v1"}
	composition := &api.RuntimeComposition{PayloadImage: "fixture.test/r@sha256:" + strings.Repeat("b", 64), Provider: provider, Schema: payload.Schema, CliVersion: cliVersion}
	base := database.RuntimeRevision{Revision: "native-base", Namespace: "kagent", AgentName: "mainloop-main", AgentUID: "agent-uid", SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, GitOrigins: []string{"github.com"}, EgressDestinations: []string{apiworkspace.ReadProxyOrigin + ":80", apiworkspace.PushProxyOrigin + ":80", "http://mainloop-mcp.mainloop.svc.cluster.local:80"}, ActorTemplateAtespace: "kagent", ActorTemplateName: "native-base-template", ActorTemplateUID: "base-template-uid"}
	require.NoError(t, store.UpsertAgentDefinition(ctx, database.AgentDefinition{Namespace: base.Namespace, AgentName: base.AgentName, AgentUID: base.AgentUID, DesiredRevision: base.Revision}))
	require.NoError(t, store.RecordRuntimeRevision(ctx, base, true))
	prepared := base
	prepared.Revision = "native-composed"
	prepared.ActorTemplateName = "native-composed-template"
	prepared.ActorTemplateUID = "native-composed-uid"
	prepared.SourceSnapshot, err = json.Marshal(database.EnvironmentRevisionSnapshot{BaseRevision: base.Revision, Environment: environment, Composition: composition})
	require.NoError(t, err)
	require.NoError(t, store.RecordRuntimeRevision(ctx, prepared, false))
	actors := &preparationActors{proxyControlledActors: &proxyControlledActors{templateUID: prepared.ActorTemplateUID}, template: &ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Atespace: prepared.ActorTemplateAtespace, Name: prepared.ActorTemplateName, Uid: prepared.ActorTemplateUID}, Containers: []*ateapipb.Container{{Name: "native", Image: environment.Image, Env: []*ateapipb.EnvVar{{Name: "KAGENT_CONFIG_JSON", Value: string(raw)}, {Name: "MAINLOOP_RUNTIME_PLATFORM", Value: environment.Platform}}}}, Volumes: []*ateapipb.Volume{{Name: "runtime-payload", Image: &ateapipb.ImageVolumeSource{Reference: composition.PayloadImage}}}}}
	actors.template.SnapshotConfig = &ateapipb.SnapshotConfig{
		OnPause:  ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
		OnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
	}
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, DevelopmentEnvironmentRegistries: []string{"fixture.test"}, CleartextMCPOrigins: []string{"http://mainloop-mcp.mainloop.svc.cluster.local"}, Credentials: []controlauth.CredentialRule{
		{Namespace: "kagent", SecretNamePattern: "mainloop-mcp-binding-*", Key: "authorization", Header: "authorization", Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Purpose: "mcp"},
		{Namespace: "kagent", SecretNamePattern: "mainloop-git-read-*", Key: "authorization", Header: "authorization", Origin: apiworkspace.ReadProxyOrigin, Purpose: "git-read"},
		{Namespace: "kagent", SecretNamePattern: "mainloop-git-push-*", Key: "authorization", Header: "authorization", Origin: apiworkspace.PushProxyOrigin, Purpose: "git-push"}}})
	require.NoError(t, err)
	policy = policy.WithRuntimePlatforms([]string{"linux/amd64"})
	token := strings.Repeat("fixture", 8)
	tokenFile := filepath.Join(t.TempDir(), "control-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(token), 0600))
	authenticator, err := authimpl.NewServiceTokenAuthenticator(tokenFile, "", policy)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	workflow := sessionsvc.NewActorWorkflow(store, actors, substrate.NewRuntimeCredentialIssuer(kube, "kagent"), "http://kagent-controller.kagent:8083")
	service := sessionsvc.NewService(store, policy, workflow, sessionsvc.WithEnvironmentPolicy(policy), sessionsvc.WithEnvironmentPreparer(fixedEnvironmentPreparer{prepared.Revision, composition}))
	listener := bufconn.Listen(DefaultMaxMessageSize)
	server, err := New(Config{Listener: listener, Authenticator: authenticator, RuntimeAuthenticator: &taskstore.Authenticator{Store: store}, SystemService: testSystemService(), SessionService: service, TaskStoreService: taskstore.NewService(store, actors)})
	require.NoError(t, err)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-serverDone) })
	conn, err := grpc.NewClient("passthrough:///native-preparation", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := api.NewSessionServiceClient(conn)
	owner := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	creation := &api.CreateSessionRequest{Agent: &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}, RequestId: "native-create", DevelopmentEnvironment: environment, Workspace: &api.Workspace{Repo: "https://github.com/owner/repo.git", Ref: sha, Branch: "feature-native", Depth: 1}, Credentials: []*api.SessionCredential{
		{Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-mcp-binding-native", Key: "authorization"}},
		{Origin: apiworkspace.ReadProxyOrigin, Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-git-read-native", Key: "authorization"}},
		{Origin: apiworkspace.PushProxyOrigin, Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-git-push-native", Key: "authorization"}}}}
	created, err := client.CreateSession(owner, creation)
	require.NoError(t, err)
	_, err = client.SuspendSession(owner, &api.SuspendSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	_, err = client.ResumeSession(owner, &api.ResumeSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	original, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	association := original.Session.RuntimeAssociation
	require.NotNil(t, association)
	setup, err := workspace.SetupDigest(profile)
	require.NoError(t, err)
	input := &api.PrepareSessionWorkspaceRequest{SessionId: created.Session.Id, ActionId: "native-create:prepare", CreateRequestId: creation.RequestId, GenerationId: association.GenerationId, ActorUid: association.ActorUid, PreparedRevision: original.Session.PreparedRevision, Workspace: proto.CloneOf(original.Session.Workspace), DevelopmentEnvironment: proto.CloneOf(environment), RuntimeComposition: proto.CloneOf(composition), SetupProfile: profile, SetupDigest: setup}
	assigned, err := client.PrepareSessionWorkspace(owner, input)
	require.NoError(t, err)
	require.Equal(t, "pending", assigned.Receipt.Classification)
	require.Error(t, store.ReserveSessionDispatch(ctx, created.Session.Id, uuid.New(), ""))
	generation, err := store.GetRuntimeGeneration(ctx, created.Session.Id)
	require.NoError(t, err)
	runtimeSecret := &corev1.Secret{}
	require.NoError(t, kube.Get(ctx, types.NamespacedName{Namespace: "kagent", Name: egress.RuntimeSecretPrefix + generation.ID.String()}, runtimeSecret))
	runtimeCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(egress.RuntimeTokenHeader, string(runtimeSecret.Data["token"])))
	runtimeCtx = context.WithValue(runtimeCtx, preparationIdentityKey{}, created.Session.Id)
	runtimeClient := preparationRuntimeClient{client: api.NewTaskStoreServiceClient(conn), ctx: runtimeCtx}
	source := workspace.FromTaskStore(runtimeClient)
	executorCtx, stopExecutor := context.WithCancel(ctx)
	t.Cleanup(stopExecutor)
	if provider == "claude" {
		var lost *api.TaskStoreServiceCompleteWorkspacePreparationRequest
		if strings.HasPrefix(scenario, "restart") {
			committed := make(chan *api.TaskStoreServiceCompleteWorkspacePreparationRequest, 1)
			source = workspace.FromTaskStore(lostPreparationReply{runtimeClient, committed})
			_, closer, err := claudeexecutor.New(executorCtx, claudeexecutor.Config{ConfigJSON: raw, DataDir: data, Environment: gitEnvironment, Workspace: source})
			require.NoError(t, err)
			select {
			case lost = <-committed:
			case <-time.After(15 * time.Second):
				t.Fatal("setup did not commit before lost reply")
			}
			stopExecutor()
			require.NoError(t, closer.Close())
			// Delete only the file this executor materialized, after its callback stopped.
			require.NoError(t, os.Remove("/tmp/kagent-claude/mcp.json"))
			if scenario == "restart-protected" {
				require.NoError(t, os.Remove("/tmp/kagent-claude/settings.json"))
			}
			executorCtx, stopExecutor = context.WithCancel(ctx)
			t.Cleanup(stopExecutor)
			recovered := make(chan *api.TaskStoreServiceCompleteWorkspacePreparationRequest, 1)
			source = workspace.FromTaskStore(capturedPreparationReply{runtimeClient, recovered})
			_, closer, err = claudeexecutor.New(executorCtx, claudeexecutor.Config{ConfigJSON: raw, DataDir: data, Environment: gitEnvironment, Workspace: source})
			require.NoError(t, err)
			t.Cleanup(func() { _ = closer.Close() })
			select {
			case result := <-recovered:
				require.True(t, proto.Equal(lost, result), "restart changed original result/time")
			case <-time.After(15 * time.Second):
				t.Fatal("restart did not reconcile original result")
			}
		} else {
			_, closer, err := claudeexecutor.New(executorCtx, claudeexecutor.Config{ConfigJSON: raw, DataDir: data, Environment: gitEnvironment, Workspace: source})
			require.NoError(t, err)
			t.Cleanup(func() { _ = closer.Close() })
		}
	} else {
		_, err := codexexecutor.New(executorCtx, codexexecutor.Config{ConfigJSON: raw, DataDir: data, Environment: gitEnvironment, Workspace: source})
		require.NoError(t, err)
	}
	var completed *api.Session
	require.Eventually(t, func() bool {
		r, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
		if err != nil {
			return false
		}
		completed = r.Session
		return completed.GetWorkspacePreparation().GetClassification() == "confirmed" || completed.GetWorkspacePreparation().GetClassification() == "definite-failure"
	}, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, created.Session.Id, completed.Id)
	require.Equal(t, created.Session.ContextId, completed.ContextId)
	require.Equal(t, original.Session.PreparedRevision, completed.PreparedRevision)
	require.True(t, proto.Equal(environment, completed.DevelopmentEnvironment))
	require.True(t, proto.Equal(composition, completed.RuntimeComposition))
	require.True(t, proto.Equal(input, completed.WorkspacePreparation.Original))
	require.Equal(t, "confirmed", completed.WorkspacePreparation.Classification)
	require.Equal(t, sha, completed.WorkspacePreparation.Head)
	require.NotNil(t, completed.WorkspacePreparation.EffectObservedAt)
	require.False(t, completed.WorkspacePreparation.Historical)
	require.NoFileExists(t, nativeLog)
	require.NoFileExists(t, filepath.Join(data, "adapter", "state.json"))
	if scenario == "lifecycle" {
		_, err = client.SuspendSession(owner, &api.SuspendSessionRequest{SessionId: created.Session.Id})
		require.NoError(t, err)
		_, err = client.ResumeSession(owner, &api.ResumeSessionRequest{SessionId: created.Session.Id})
		require.NoError(t, err)
		resumed, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
		require.NoError(t, err)
		require.Equal(t, association.GenerationId, resumed.Session.RuntimeAssociation.GenerationId)
		require.Equal(t, association.ActorUid, resumed.Session.RuntimeAssociation.ActorUid)
	}
	// Exercise the real interaction service's preparation gate without a native send.
	principal, err := authenticator.Authenticate(ctx, http.Header{"Authorization": []string{"Bearer " + token}}, nil)
	require.NoError(t, err)
	authCtx := auth.AuthSessionTo(ctx, principal)
	interactions := sessionsvc.NewInteractionService(store, nil, service)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("sole owner brief"))
	message.ContextID = created.Session.Id
	send, err := interactions.PrepareSend(authCtx, types.NamespacedName{Namespace: "kagent", Name: "mainloop-main"}, &a2a.SendMessageRequest{Message: message})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, send.DispatchID)
	revoked, err := interactions.RevokeSend(authCtx, types.NamespacedName{Namespace: "kagent", Name: "mainloop-main"}, message, send.DispatchID)
	require.NoError(t, err)
	require.True(t, revoked)
	// Cached GET cannot refresh native observation. The original effect time is immutable.
	cached, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	require.True(t, proto.Equal(completed.WorkspacePreparation, cached.Session.WorkspacePreparation))
	challenge, err := client.PrepareSessionWorkspace(owner, input)
	require.NoError(t, err)
	require.Equal(t, uint64(1), challenge.Receipt.ObservationSequence)
	require.Nil(t, challenge.Receipt.ObservedAt)
	require.Eventually(t, func() bool {
		r, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
		return err == nil && r.Session.GetWorkspacePreparation().GetClassification() == "confirmed"
	}, 5*time.Second, 20*time.Millisecond)
	reobserved, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
	require.NoError(t, err)
	require.True(t, proto.Equal(completed.WorkspacePreparation.EffectObservedAt, reobserved.Session.WorkspacePreparation.EffectObservedAt))
	require.True(t, reobserved.Session.WorkspacePreparation.ObservedAt.AsTime().After(completed.WorkspacePreparation.ObservedAt.AsTime()))
	for _, mutation := range []string{"creator", "Agent", "namespace", "D", "R", "generation", "UID", "request", "setup"} {
		t.Run(mutation, func(t *testing.T) {
			changed := proto.CloneOf(input)
			switch mutation {
			case "D":
				changed.DevelopmentEnvironment.Image = "fixture.test/d@sha256:" + strings.Repeat("c", 64)
			case "R":
				changed.RuntimeComposition.PayloadImage = "different"
			case "generation":
				changed.GenerationId = uuid.NewString()
			case "UID":
				changed.ActorUid = "other"
			case "request":
				changed.CreateRequestId = "other"
			case "setup":
				changed.SetupDigest = strings.Repeat("0", 64)
			default:
				foreign := proto.CloneOf(completed)
				foreign.Id = uuid.NewString()
				foreign.Creator = "other"
				if mutation != "creator" {
					foreign.Creator = "mainloop"
					if mutation == "Agent" {
						foreign.Agent.Name = "other"
					} else {
						foreign.Agent.Namespace = "other"
					}
				}
				// Service policy checks trusted stored attributes; no forged request Agent exists.
				require.Error(t, policy.CheckSession(ctx, foreign))
				return
			}
			_, err := client.PrepareSessionWorkspace(owner, changed)
			require.Error(t, err)
		})
	}
	// Public/control tokens cannot invoke the private assignment/completion channel.
	_, err = api.NewTaskStoreServiceClient(conn).GetWorkspace(owner, &api.TaskStoreServiceGetWorkspaceRequest{SessionId: created.Session.Id})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	// An actual wrong HEAD behind the done marker is a failed observation, never reset.
	command := exec.Command("git", "-C", filepath.Join(data, "workspace"), "-c", "commit.gpgsign=false", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "changed HEAD")
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	require.NoError(t, command.Run())
	_, err = client.PrepareSessionWorkspace(owner, input)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		r, err := client.GetSession(owner, &api.GetSessionRequest{SessionId: created.Session.Id})
		return err == nil && r.Session.GetWorkspacePreparation().GetClassification() == "definite-failure"
	}, 5*time.Second, 20*time.Millisecond)
	require.Error(t, store.ReserveSessionDispatch(ctx, created.Session.Id, uuid.New(), ""))
	require.NoFileExists(t, nativeLog)
}
