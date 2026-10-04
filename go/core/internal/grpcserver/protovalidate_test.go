package grpcserver

import (
	"context"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	protovalidatemiddleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestProtovalidateUnaryInterceptor(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}

	handled := false
	_, err = protovalidatemiddleware.UnaryServerInterceptor(validator)(
		t.Context(),
		&apiv1alpha1.CreateSessionRequest{},
		&grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) {
			handled = true
			return nil, nil
		},
	)
	if handled {
		t.Fatal("handler called for invalid request")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("validation code = %v, want %v", status.Code(err), codes.InvalidArgument)
	}
	if details := status.Convert(err).Details(); len(details) != 1 {
		t.Fatalf("validation details = %d, want 1", len(details))
	}
}

func TestSessionRequestValidation(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		request proto.Message
		valid   bool
	}{
		{"ordinary name", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Name: "Deploy 🚀"}, true},
		{"list without namespace", &apiv1alpha1.ListSessionsRequest{}, true},
		{"workspace", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/oldsj/testrepo", Ref: "main", Branch: "spike-b-test", Depth: 1}}, true},
		{"workspace without ref or depth", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/oldsj/testrepo"}}, true},
		{"workspace without repo", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Ref: "main"}}, false},
		{"workspace repo is not a URI", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "not a uri"}}, false},
		{"workspace option-like ref", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/o/r", Ref: "--upload-pack=x"}}, false},
		{"workspace branch with a space", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/o/r", Branch: "a b"}}, false},
		{"workspace negative depth", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/o/r", Depth: -1}}, false},
		{"workspace excessive depth", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/o/r", Depth: 1001}}, false},
		{"runtime workspace read", &apiv1alpha1.TaskStoreServiceGetWorkspaceRequest{SessionId: "11111111-1111-4111-8111-111111111111"}, true},
		{"runtime workspace read bad id", &apiv1alpha1.TaskStoreServiceGetWorkspaceRequest{SessionId: "nope"}, false},
		{"missing target namespace", &apiv1alpha1.ListSessionsRequest{Agent: &apiv1alpha1.ResourceReference{Name: "assistant"}}, false},
		{"leading whitespace", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Name: " title"}, false},
		{"control character", &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "request-1", Name: "first\nsecond"}, false},
		{"invalid template filter", &apiv1alpha1.ListSessionsRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "NOT A NAME"}}, false},
		{"valid rename", &apiv1alpha1.UpdateSessionNameRequest{SessionId: "11111111-1111-4111-8111-111111111111", Name: "New title"}, true},
		{"invalid rename id", &apiv1alpha1.UpdateSessionNameRequest{SessionId: "not-a-uuid", Name: "New title"}, false},
		{"valid checkpoint rename", &apiv1alpha1.UpdateCheckpointNameRequest{CheckpointId: "11111111-1111-4111-8111-111111111111", Name: "Before the detour"}, true},
		{"empty checkpoint rename", &apiv1alpha1.UpdateCheckpointNameRequest{CheckpointId: "11111111-1111-4111-8111-111111111111"}, true},
		{"checkpoint without selected task", &apiv1alpha1.CreateCheckpointRequest{SessionId: "11111111-1111-4111-8111-111111111111", RequestId: "request"}, false},
		{"checkpoint selected task", &apiv1alpha1.CreateCheckpointRequest{SessionId: "11111111-1111-4111-8111-111111111111", RequestId: "request", ExpectedHeadTaskId: "task"}, true},
		{"checkpoint rename control character", &apiv1alpha1.UpdateCheckpointNameRequest{CheckpointId: "11111111-1111-4111-8111-111111111111", Name: "first\nsecond"}, false},
		{"share without ttl", &apiv1alpha1.CreateSessionShareRequest{SessionId: "11111111-1111-4111-8111-111111111111", Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE}, true},
		{"share positive ttl", &apiv1alpha1.CreateSessionShareRequest{SessionId: "11111111-1111-4111-8111-111111111111", Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE, Ttl: durationpb.New(time.Hour)}, true},
		{"share zero ttl", &apiv1alpha1.CreateSessionShareRequest{SessionId: "11111111-1111-4111-8111-111111111111", Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE, Ttl: durationpb.New(0)}, false},
		{"share negative ttl", &apiv1alpha1.CreateSessionShareRequest{SessionId: "11111111-1111-4111-8111-111111111111", Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE, Ttl: durationpb.New(-time.Second)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validator.Validate(test.request)
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func TestInvalidSessionAndCheckpointIDsNeverReachHandlers(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	requests := []proto.Message{
		&apiv1alpha1.GetSessionRequest{SessionId: "invalid"},
		&apiv1alpha1.UpdateSessionNameRequest{SessionId: "invalid"},
		&apiv1alpha1.SuspendSessionRequest{SessionId: "invalid"},
		&apiv1alpha1.ResumeSessionRequest{SessionId: "invalid"},
		&apiv1alpha1.DeleteSessionRequest{SessionId: "invalid"},
		&apiv1alpha1.CreateSessionShareRequest{SessionId: "invalid", Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_ONLY},
		&apiv1alpha1.ListSessionSharesRequest{SessionId: "invalid"},
		&apiv1alpha1.RevokeSessionShareRequest{ShareId: "invalid"},
		&apiv1alpha1.CreateCheckpointRequest{SessionId: "invalid", RequestId: "request"},
		&apiv1alpha1.GetCheckpointRequest{CheckpointId: "invalid"},
		&apiv1alpha1.ListCheckpointsRequest{SessionId: "invalid"},
		&apiv1alpha1.DeleteCheckpointRequest{CheckpointId: "invalid"},
		&apiv1alpha1.ForkSessionRequest{CheckpointId: "invalid", RequestId: "request"},
		&apiv1alpha1.UpdateCheckpointNameRequest{CheckpointId: "invalid"},
	}
	for _, request := range requests {
		t.Run(string(proto.MessageName(request)), func(t *testing.T) {
			_, err := protovalidatemiddleware.UnaryServerInterceptor(validator)(
				t.Context(), request, &grpc.UnaryServerInfo{},
				func(context.Context, any) (any, error) {
					t.Fatal("handler called with an invalid UUID")
					return nil, nil
				},
			)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("validation code = %v, want %v", status.Code(err), codes.InvalidArgument)
			}
		})
	}
}

func TestAgentRequestValidation(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	ref := &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}
	resource := &apiv1alpha1.StructuredObject{}
	for _, test := range []struct {
		name    string
		request proto.Message
		valid   bool
	}{
		{"list", &apiv1alpha1.ListAgentsRequest{Namespace: "team-a"}, true},
		{"list missing namespace", &apiv1alpha1.ListAgentsRequest{}, false},
		{"list invalid namespace", &apiv1alpha1.ListAgentsRequest{Namespace: "team/a"}, false},
		{"get", &apiv1alpha1.GetAgentRequest{Ref: ref}, true},
		{"get missing ref", &apiv1alpha1.GetAgentRequest{}, false},
		{"get invalid ref", &apiv1alpha1.GetAgentRequest{Ref: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "INVALID"}}, false},
		{"create", &apiv1alpha1.CreateAgentRequest{Ref: ref, Resource: resource}, true},
		{"create missing ref", &apiv1alpha1.CreateAgentRequest{Resource: resource}, false},
		{"create missing resource", &apiv1alpha1.CreateAgentRequest{Ref: ref}, false},
		{"update", &apiv1alpha1.UpdateAgentRequest{Ref: ref, Resource: resource}, true},
		{"update missing ref", &apiv1alpha1.UpdateAgentRequest{Resource: resource}, false},
		{"update missing resource", &apiv1alpha1.UpdateAgentRequest{Ref: ref}, false},
		{"delete", &apiv1alpha1.DeleteAgentRequest{Ref: ref}, true},
		{"delete missing ref", &apiv1alpha1.DeleteAgentRequest{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			handled := false
			_, err := protovalidatemiddleware.UnaryServerInterceptor(validator)(
				t.Context(), test.request, &grpc.UnaryServerInfo{},
				func(context.Context, any) (any, error) {
					handled = true
					return nil, nil
				},
			)
			if handled != test.valid {
				t.Fatalf("handler called = %t, want %t", handled, test.valid)
			}
			want := codes.InvalidArgument
			if test.valid {
				want = codes.OK
			}
			if status.Code(err) != want {
				t.Fatalf("validation code = %v, want %v", status.Code(err), want)
			}
		})
	}
}

func TestSessionCredentialRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name, origin, header, secret, key string
		count                             int
		valid                             bool
	}{
		{"HTTP", "http://mcp.test.svc.cluster.local", "Authorization", "tokens", "binding-id", 1, true},
		{"HTTPS", "https://mcp.example.com:443", "authorization", "tokens", "binding-id", 1, true},
		{"path", "http://mcp.example.com/mcp", "authorization", "tokens", "key", 1, false},
		{"query", "http://mcp.example.com?x=1", "authorization", "tokens", "key", 1, false},
		{"header", "http://mcp.example.com", "bad header", "tokens", "key", 1, false},
		{"missing Secret name", "http://mcp.example.com", "authorization", "", "key", 1, false},
		{"invalid key", "http://mcp.example.com", "authorization", "tokens", "../key", 1, false},
		{"too many", "http://mcp.example.com", "authorization", "tokens", "key", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &apiv1alpha1.CreateSessionRequest{Agent: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"}, RequestId: "credential-test"}
			for range tc.count {
				request.Credentials = append(request.Credentials, &apiv1alpha1.SessionCredential{Origin: tc.origin, Header: tc.header, SecretRef: &apiv1alpha1.SecretKeyReference{Name: tc.secret, Key: tc.key}})
			}
			err := protovalidate.Validate(request)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
