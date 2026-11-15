package session

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
)

func dotfilesRequest() *apiv1.CreateSessionRequest {
	return &apiv1.CreateSessionRequest{
		Agent:     &apiv1.ResourceReference{Namespace: "team-a", Name: "assistant"},
		RequestId: "request-1",
		Credentials: []*apiv1.SessionCredential{{
			Origin: "https://mcp.example.test", Header: "x-mcp-token",
			SecretRef: &apiv1.SecretKeyReference{Name: "session-tokens", Key: "mcp"},
		}},
		Dotfiles: &apiv1.Dotfiles{
			Url:    "https://mcp.example.test/dotfiles/sha256:" + strings.Repeat("a", 64),
			Digest: "sha256:" + strings.Repeat("a", 64), Repository: "example/agent-dotfiles",
			Commit: strings.Repeat("c", 40), Path: "home/.agents",
		},
	}
}

func TestCreateFromRecordsTheDotfiles(t *testing.T) {
	store := &serviceTestStore{}
	service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{})
	request := dotfilesRequest()
	if _, err := service.CreateFrom(serviceTestContext("alice"), request); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(store.createInput.GetDotfiles(), request.GetDotfiles()) {
		t.Fatalf("recorded dotfiles = %v", store.createInput.GetDotfiles())
	}
}

func TestCreateFromRefusesDotfilesWithoutACredentialForTheirOrigin(t *testing.T) {
	for name, change := range map[string]func(*apiv1.CreateSessionRequest){
		"no credential":    func(r *apiv1.CreateSessionRequest) { r.Credentials = nil },
		"another origin":   func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Url = "https://elsewhere.example.test/dotfiles/x" },
		"another port":     func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Url = "https://mcp.example.test:8443/dotfiles/x" },
		"user info":        func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Url = "https://u:p@mcp.example.test/dotfiles/x" },
		"query":            func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Url = "https://mcp.example.test/dotfiles/x?y=1" },
		"malformed digest": func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Digest = "md5:abc" },
		"short commit":     func(r *apiv1.CreateSessionRequest) { r.Dotfiles.Commit = "abc123" },
	} {
		t.Run(name, func(t *testing.T) {
			store := &serviceTestStore{}
			service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{})
			request := dotfilesRequest()
			change(request)
			_, err := service.CreateFrom(serviceTestContext("alice"), request)
			if !serviceerrors.IsCode(err, serviceerrors.CodeInvalidArgument) {
				t.Fatalf("CreateFrom() error = %v, want invalid argument", err)
			}
			if store.createInput != nil {
				t.Fatal("a refused Session was reserved")
			}
		})
	}
}
