package session

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
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

func dotfilesUpdate() (*apiv1.Session, *apiv1.UpdateSessionDotfilesRequest) {
	create := dotfilesRequest()
	session := &apiv1.Session{Id: "11111111-1111-4111-8111-111111111111", Credentials: create.GetCredentials(), Dotfiles: create.GetDotfiles()}
	next := proto.Clone(create.GetDotfiles()).(*apiv1.Dotfiles)
	next.Digest = "sha256:" + strings.Repeat("b", 64)
	next.Url = "https://mcp.example.test/dotfiles/" + next.Digest
	next.Commit = strings.Repeat("d", 40)
	return session, &apiv1.UpdateSessionDotfilesRequest{SessionId: session.GetId(), Dotfiles: next, PreviousDigest: create.GetDotfiles().GetDigest()}
}

func TestUpdateDotfilesChecksTheSessionsCredentials(t *testing.T) {
	session, request := dotfilesUpdate()
	store := &serviceTestStore{dotfilesSession: session}
	service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{})
	updated, err := service.UpdateDotfiles(serviceTestContext("alice"), request)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(updated.GetDotfiles(), request.GetDotfiles()) || store.dotfilesUserID != "alice" {
		t.Fatalf("updated = %v by %q", updated.GetDotfiles(), store.dotfilesUserID)
	}

	for name, change := range map[string]func(*apiv1.Session, *apiv1.UpdateSessionDotfilesRequest){
		"a fork, which has no credentials": func(s *apiv1.Session, _ *apiv1.UpdateSessionDotfilesRequest) { s.Credentials = nil },
		"another origin": func(_ *apiv1.Session, r *apiv1.UpdateSessionDotfilesRequest) {
			r.Dotfiles.Url = "https://elsewhere.example.test/dotfiles/x"
		},
		"no dotfiles":               func(_ *apiv1.Session, r *apiv1.UpdateSessionDotfilesRequest) { r.Dotfiles = nil },
		"a malformed previous":      func(_ *apiv1.Session, r *apiv1.UpdateSessionDotfilesRequest) { r.PreviousDigest = "abc" },
		"a malformed session id":    func(_ *apiv1.Session, r *apiv1.UpdateSessionDotfilesRequest) { r.SessionId = "x" },
		"a malformed digest to set": func(_ *apiv1.Session, r *apiv1.UpdateSessionDotfilesRequest) { r.Dotfiles.Digest = "md5:x" },
	} {
		t.Run(name, func(t *testing.T) {
			session, request := dotfilesUpdate()
			change(session, request)
			before := proto.Clone(session.GetDotfiles())
			store := &serviceTestStore{dotfilesSession: session}
			service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{})
			if _, err := service.UpdateDotfiles(serviceTestContext("alice"), request); !serviceerrors.IsCode(err, serviceerrors.CodeInvalidArgument) {
				t.Fatalf("UpdateDotfiles() error = %v, want invalid argument", err)
			}
			if !proto.Equal(session.GetDotfiles(), before) {
				t.Fatal("a refused update changed the Session")
			}
		})
	}
}

func TestUpdateDotfilesMapsStoreErrors(t *testing.T) {
	for _, tt := range []struct {
		err  error
		code serviceerrors.Code
	}{
		{database.ErrConflict, serviceerrors.CodeFailedPrecondition},
		{database.ErrNotFound, serviceerrors.CodeNotFound},
	} {
		session, request := dotfilesUpdate()
		store := &serviceTestStore{dotfilesSession: session, dotfilesErr: fmt.Errorf("update: %w", tt.err)}
		service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{})
		if _, err := service.UpdateDotfiles(serviceTestContext("alice"), request); !serviceerrors.IsCode(err, tt.code) {
			t.Fatalf("UpdateDotfiles() with %v = %v, want %v", tt.err, err, tt.code)
		}
	}
}
