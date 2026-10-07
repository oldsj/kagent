package grpcserver

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
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

type environmentGRPCStore struct {
	*database.Client
	created *apiv1alpha1.Session
	lookups int
}

func (s *environmentGRPCStore) CreateSession(_ context.Context, value *apiv1alpha1.Session, _ string) (*apiv1alpha1.Session, bool, error) {
	s.created = proto.CloneOf(value)
	return value, true, nil
}
func (s *environmentGRPCStore) LookupSessionCreation(context.Context, string, string) (*apiv1alpha1.Session, error) {
	s.lookups++
	return nil, database.ErrNotFound
}

type environmentGRPCPreparer struct {
	seen *apiv1alpha1.DevelopmentEnvironment
}

func (p *environmentGRPCPreparer) Prepare(_ context.Context, _ *apiv1alpha1.ResourceReference, selection *apiv1alpha1.DevelopmentEnvironment) (string, *apiv1alpha1.RuntimeComposition, error) {
	p.seen = proto.CloneOf(selection)
	return "prepared", &apiv1alpha1.RuntimeComposition{PayloadImage: "operator-payload", Provider: "claude", Schema: 1}, nil
}

var _ sessionsvc.EnvironmentPreparer = (*environmentGRPCPreparer)(nil)

type environmentGRPCWorkflow struct {
	credentialTestWorkflow
	creates int
}

func (w *environmentGRPCWorkflow) Create(_ context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	w.creates++
	return session, nil
}

func TestSessionEnvironmentThroughGRPC(t *testing.T) {
	for _, mode := range []string{"insecure", "trusted-proxy", "service-token"} {
		t.Run(mode, func(t *testing.T) {
			store := &environmentGRPCStore{}
			preparer := &environmentGRPCPreparer{}
			workflow := &environmentGRPCWorkflow{}
			policy, err := controlauth.New(controlauth.Config{Namespace: "agents", Agents: []string{"writer"}, DevelopmentEnvironmentRegistries: []string{"registry"}})
			require.NoError(t, err)
			policy = policy.WithRuntimePlatforms([]string{"linux/arm64"})
			var authenticator auth.AuthProvider = &authimpl.InsecureAuthenticator{}
			var authorizer auth.CollectionAuthorizer = &auth.NoopAuthorizer{}
			// Intentionally install the trusted policy even in the untrusted modes:
			// a spoofed mainloop user still lacks verified service identity.
			options := []sessionsvc.Option{sessionsvc.WithEnvironmentPreparer(preparer), sessionsvc.WithEnvironmentPolicy(policy)}
			headers := metadata.Pairs("x-user-id", "mainloop")
			switch mode {
			case "trusted-proxy":
				authenticator = authimpl.NewProxyAuthenticator("sub")
				claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"mainloop","service":"mainloop"}`))
				headers.Set("authorization", "Bearer e30."+claims+".signature")
			case "service-token":
				token := strings.Repeat("a", 32)
				file := filepath.Join(t.TempDir(), "token")
				require.NoError(t, os.WriteFile(file, []byte(token), 0600))
				authenticator, err = authimpl.NewServiceTokenAuthenticator(file, "", policy)
				require.NoError(t, err)
				authorizer = policy
				headers.Set("authorization", "Bearer "+token)
			}
			listener := bufconn.Listen(DefaultMaxMessageSize)
			server, err := New(Config{Listener: listener, Authenticator: authenticator, SystemService: testSystemService(), SessionService: sessionsvc.NewService(store, authorizer, workflow, options...)})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- server.Start(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
			conn, err := grpc.NewClient("passthrough:///environment-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			client := apiv1alpha1.NewSessionServiceClient(conn)
			owner := metadata.NewOutgoingContext(t.Context(), headers)
			request := &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "agents", Name: "writer"}, RequestId: "composed-request", DevelopmentEnvironment: &apiv1alpha1.DevelopmentEnvironment{Image: "registry/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "accepted-v1"}}
			response, err := client.CreateSession(owner, request)
			if mode != "service-token" {
				require.Equal(t, codes.PermissionDenied, status.Code(err))
				require.Nil(t, preparer.seen)
				require.Nil(t, store.created)
				require.Zero(t, store.lookups)
				require.Zero(t, workflow.creates)
				return
			}
			require.NoError(t, err)
			require.True(t, proto.Equal(request.DevelopmentEnvironment, preparer.seen))
			require.True(t, proto.Equal(request.DevelopmentEnvironment, response.Session.DevelopmentEnvironment))
			require.Equal(t, "operator-payload", response.Session.RuntimeComposition.PayloadImage)
			for _, field := range []string{"tag", "platform", "policy", "registry", "unknown platform"} {
				invalid := proto.CloneOf(request)
				code := codes.InvalidArgument
				switch field {
				case "tag":
					invalid.DevelopmentEnvironment.Image = "registry/dev:latest"
				case "platform":
					invalid.DevelopmentEnvironment.Platform = "linux/mips"
				case "policy":
					invalid.DevelopmentEnvironment.PolicyIdentity = ""
				case "registry":
					invalid.DevelopmentEnvironment.Image = "registry.evil/dev@sha256:" + strings.Repeat("a", 64)
					code = codes.PermissionDenied
				case "unknown platform":
					invalid.DevelopmentEnvironment.Platform = "linux/amd64"
					code = codes.PermissionDenied
				}
				store.created, preparer.seen, store.lookups = nil, nil, 0
				workflow.creates = 0
				_, err = client.CreateSession(owner, invalid)
				require.Equal(t, code, status.Code(err), field)
				require.Nil(t, store.created)
				require.Nil(t, preparer.seen)
				require.Zero(t, store.lookups)
				require.Zero(t, workflow.creates)
			}
		})
	}
}
