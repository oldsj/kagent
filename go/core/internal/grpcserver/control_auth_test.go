package grpcserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	pkgauth "github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionalpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type controlStore struct {
	*database.Client
	calls int
}

func (s *controlStore) CreateSession(_ context.Context, session *api.Session, _ string) (*api.Session, bool, error) {
	s.calls++
	return session, true, nil
}

func TestControlAuthNativeAndWeb(t *testing.T) {
	token := strings.Repeat("s", 32)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(token), 0600))
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	store := &controlStore{}
	listener := bufconn.Listen(DefaultMaxMessageSize)
	policies := DefaultMethodPolicies()
	const futurePublic = "/test.Public/Discover"
	policies[futurePublic] = pkgauth.AccessPublic
	server, err := New(Config{Listener: listener, Authenticator: provider, Reflection: true, MethodPolicies: policies, RegisterServices: func(registrar grpc.ServiceRegistrar) {
		registrar.RegisterService(&grpc.ServiceDesc{ServiceName: "test.Public", HandlerType: (*any)(nil), Methods: []grpc.MethodDesc{{MethodName: "Discover"}}}, &struct{}{})
	}, SystemService: testSystemService(), SessionService: sessionsvc.NewService(store, policy, credentialTestWorkflow{})})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	conn, err := grpc.NewClient("passthrough:///control", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := api.NewSessionServiceClient(conn)
	request := &api.CreateSessionRequest{Agent: &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}, RequestId: "test"}
	for _, tc := range []struct {
		name    string
		headers []string
		code    codes.Code
	}{
		{"forged", []string{"x-user-id", "mainloop", "x-agent-name", "kagent/mainloop-main"}, codes.Unauthenticated},
		{"bad", []string{"authorization", "Bearer " + strings.Repeat("b", 32)}, codes.Unauthenticated},
		{"duplicate", []string{"authorization", "Bearer " + token, "authorization", "Bearer " + token}, codes.Unauthenticated},
		{"allowed", []string{"authorization", "Bearer " + token, "x-user-id", "other", "x-agent-name", "other/agent"}, codes.OK},
	} {
		t.Run("native/"+tc.name, func(t *testing.T) {
			before := store.calls
			resp, err := client.CreateSession(metadata.NewOutgoingContext(t.Context(), metadata.Pairs(tc.headers...)), request)
			require.Equal(t, tc.code, status.Code(err))
			if tc.code == codes.OK {
				require.Equal(t, "mainloop", resp.Session.Creator)
				require.Equal(t, before+1, store.calls)
			} else {
				require.Equal(t, before, store.calls)
			}
		})
	}
	owner := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+token))
	_, err = api.NewSystemServiceClient(conn).GetCurrentUser(owner, &api.GetCurrentUserRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	for _, tc := range []struct {
		name    string
		headers []string
		status  string
	}{
		{"missing", nil, "16"}, {"duplicate", []string{"Bearer " + token, "Bearer " + token}, "16"}, {"allowed", []string{"Bearer " + token}, "0"},
	} {
		t.Run("grpc-web/"+tc.name, func(t *testing.T) {
			payload, err := proto.Marshal(request)
			require.NoError(t, err)
			body := make([]byte, 5+len(payload))
			binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
			copy(body[5:], payload)
			req := httptest.NewRequest(http.MethodPost, "http://controller"+api.SessionService_CreateSession_FullMethodName, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/grpc-web+proto")
			req.Header.Set("X-Grpc-Web", "1")
			req.Header.Set("X-User-Id", "mainloop")
			for _, header := range tc.headers {
				req.Header.Add("Authorization", header)
			}
			recorder := httptest.NewRecorder()
			before := store.calls
			server.WebHandler().ServeHTTP(recorder, req)
			if recorder.Header().Get("Grpc-Status") != tc.status {
				require.Contains(t, recorder.Body.String(), "grpc-status: "+tc.status)
			}
			if tc.status == "0" {
				require.Equal(t, before+1, store.calls)
			} else {
				require.Equal(t, before, store.calls)
			}
		})
	}
	// Enumerate the actual server surface, including extensions, rather than
	// trusting public classifications or assuming the policy map is exhaustive.
	for serviceName, descriptor := range server.server.GetServiceInfo() {
		require.NotContains(t, serviceName, "grpc.reflection.")
		for _, method := range descriptor.Methods {
			fullMethod := "/" + serviceName + "/" + method.Name
			switch fullMethod {
			case api.SystemService_GetVersion_FullMethodName, grpc_health_v1.Health_Check_FullMethodName, grpc_health_v1.Health_List_FullMethodName, grpc_health_v1.Health_Watch_FullMethodName:
				continue
			}
			if controlauth.CheckMethod(fullMethod) == nil {
				continue
			}
			called := false
			ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer "+token))
			expected := codes.PermissionDenied
			// TaskStore keeps its independent authenticator; the control credential
			// cannot replace it. This fixture intentionally has no runtime credentials.
			if policies[fullMethod] == pkgauth.AccessRuntime {
				expected = codes.Unauthenticated
			}
			var err error
			if method.IsClientStream || method.IsServerStream {
				err = authenticationStreamInterceptor(provider, nil, nil, policies)(nil, &controlAuthStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: fullMethod}, func(any, grpc.ServerStream) error { called = true; return nil })
			} else {
				_, err = authenticationUnaryInterceptor(provider, nil, nil, policies)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			}
			require.Equal(t, expected, status.Code(err), fullMethod)
			require.False(t, called, fullMethod)
		}
	}
}

// A stream must be refused before its handler receives the first message.
type controlAuthStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *controlAuthStream) Context() context.Context { return s.ctx }
func TestControlStreamMethodGuard(t *testing.T) {
	provider := &testAuthenticator{session: &testSession{principal: pkgauth.Principal{Service: "mainloop", User: pkgauth.User{ID: "mainloop"}}}}
	for _, tc := range []struct {
		method  string
		allowed bool
	}{
		{a2apb.A2AService_SendStreamingMessage_FullMethodName, true},
		{a2apb.A2AService_SubscribeToTask_FullMethodName, true},
		{"/future.Service/Stream", false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			called := false
			policies := DefaultMethodPolicies()
			policies[tc.method] = pkgauth.AccessRead
			err := authenticationStreamInterceptor(provider, nil, nil, policies)(nil, &controlAuthStream{ctx: t.Context()}, &grpc.StreamServerInfo{FullMethod: tc.method}, func(_ any, stream grpc.ServerStream) error {
				called = true
				session, ok := pkgauth.AuthSessionFrom(stream.Context())
				require.True(t, ok)
				require.Equal(t, "mainloop", session.Principal().Service)
				return nil
			})
			require.Equal(t, tc.allowed, called)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.Equal(t, codes.PermissionDenied, status.Code(err))
			}
		})
	}
}

func TestReflectionServiceTokenIsolation(t *testing.T) {
	token := strings.Repeat("r", 32)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(token), 0600))
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	for _, mode := range []struct {
		name     string
		provider pkgauth.AuthProvider
		disabled bool
	}{
		{"service-token", provider, true}, {"insecure", &authimpl.InsecureAuthenticator{}, false}, {"trusted-proxy", authimpl.NewProxyAuthenticator(""), false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			listener := bufconn.Listen(DefaultMaxMessageSize)
			server, err := New(Config{Listener: listener, Authenticator: mode.provider, Reflection: true, SystemService: testSystemService()})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- server.Start(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
			conn, err := grpc.NewClient("passthrough:///reflection", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			for _, credential := range []struct{ name, value string }{{"none", ""}, {"invalid", "Bearer " + strings.Repeat("x", 32)}, {"valid", "Bearer " + token}} {
				for _, version := range []string{"v1", "v1alpha"} {
					t.Run(version+"/"+credential.name, func(t *testing.T) {
						ctx := t.Context()
						if credential.value != "" {
							ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", credential.value))
						}
						var receiveErr error
						if version == "v1" {
							stream, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
							require.NoError(t, err)
							err = stream.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{ListServices: ""}})
							require.True(t, err == nil || err == io.EOF)
							response, err := stream.Recv()
							receiveErr = err
							if mode.disabled {
								require.Nil(t, response)
							} else {
								require.NotEmpty(t, response.GetListServicesResponse().GetService())
							}
							require.NoError(t, stream.CloseSend())
						} else {
							stream, err := reflectionalpha.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
							require.NoError(t, err)
							err = stream.Send(&reflectionalpha.ServerReflectionRequest{MessageRequest: &reflectionalpha.ServerReflectionRequest_ListServices{ListServices: ""}}) //nolint:staticcheck // Test the supported legacy v1alpha endpoint as required by the isolation contract.
							require.True(t, err == nil || err == io.EOF)
							response, err := stream.Recv()
							receiveErr = err
							if mode.disabled {
								require.Nil(t, response)
							} else {
								require.NotEmpty(t, response.GetListServicesResponse().GetService()) //nolint:staticcheck // Assert the legacy v1alpha response remains available in existing auth modes.
							}
							require.NoError(t, stream.CloseSend())
						}
						if mode.disabled {
							require.Equal(t, codes.Unimplemented, status.Code(receiveErr))
						} else {
							require.NoError(t, receiveErr)
						}
					})
				}
			}
		})
	}
}
