package grpcserver

import (
	"context"
	"net"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// Runtime creation is tested by the Session workflow suite. This test exercises
// transport, validation, authentication, idempotency and real persistence.
type credentialTestWorkflow struct{ *sessionsvc.ActorWorkflow }

func (credentialTestWorkflow) Create(_ context.Context, session *apiv1.Session) (*apiv1.Session, error) {
	return session, nil
}

func TestSessionCredentialsThroughGRPC(t *testing.T) {
	dsn := dbtest.StartT(context.WithoutCancel(t.Context()), t)
	dbtest.MigrateT(t, dsn, false)
	db, err := database.Connect(t.Context(), &database.PostgresConfig{Pool: dbtest.PoolConfig(dsn)})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	store := database.NewClient(db, "public")
	definition := database.AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "agent-uid", DesiredRevision: "credential-revision"}
	require.NoError(t, store.UpsertAgentDefinition(t.Context(), definition))
	_, err = store.RecordRuntimeRevision(t.Context(), database.RuntimeRevision{Revision: definition.DesiredRevision, Namespace: definition.Namespace, AgentName: definition.AgentName, AgentUID: definition.AgentUID, SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, EgressDestinations: []string{"http://echo.test.svc.cluster.local:80"}, ActorTemplateAtespace: "team-a", ActorTemplateName: "credential-template", ActorTemplateUID: "template-uid"}, database.RecordRevisionOptions{Ready: true})
	require.NoError(t, err)
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
	client := apiv1.NewSessionServiceClient(conn)
	owner := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-user-id", "alice"))
	request := &apiv1.CreateSessionRequest{Agent: &apiv1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "credential-create", Credentials: []*apiv1.SessionCredential{{Origin: "http://echo.test.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1.SecretKeyReference{Name: "tokens", Key: "binding-one"}}}}
	created, err := client.CreateSession(owner, request)
	require.NoError(t, err)
	get, err := client.GetSession(owner, &apiv1.GetSessionRequest{SessionId: created.Session.Id})
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
