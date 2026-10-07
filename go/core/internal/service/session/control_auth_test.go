package session

import (
	"fmt"
	"testing"

	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
)

type mainloopSession struct{}

func (mainloopSession) Principal() auth.Principal {
	return auth.Principal{Service: "mainloop", User: auth.User{ID: "mainloop"}}
}

func TestControlSessionScopeBeforeEffects(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, Credentials: []controlauth.CredentialRule{{Namespace: "kagent", SecretNamePattern: "mainloop-mcp-binding-one", Key: "authorization", Origin: "https://mainloop.example.com", Header: "authorization", Purpose: "mcp"}}})
	require.NoError(t, err)
	ctx := auth.AuthSessionTo(t.Context(), mainloopSession{})
	agent := &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}
	for _, tc := range []struct {
		name        string
		agent       *api.ResourceReference
		credentials []*api.SessionCredential
	}{
		{"other Agent", &api.ResourceReference{Namespace: "kagent", Name: "other"}, nil},
		{"other namespace", &api.ResourceReference{Namespace: "other", Name: "mainloop-main"}, nil},
		{"Secret", agent, []*api.SessionCredential{{Origin: "https://mainloop.example.com", Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "database", Key: "password"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &serviceTestStore{}
			service := NewService(store, policy, serviceTestWorkflow{})
			_, err := service.Create(ctx, tc.agent, "request", "", nil, tc.credentials...)
			require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
			require.Nil(t, store.createInput)
		})
	}
	store := &serviceTestStore{}
	service := NewService(store, policy, serviceTestWorkflow{})
	created, err := service.Create(ctx, agent, "allowed", "", nil)
	require.NoError(t, err)
	require.Equal(t, "mainloop", created.Creator)
	approved, err := service.Create(ctx, agent, "approved", "", nil, &api.SessionCredential{Origin: "https://mainloop.example.com", Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-mcp-binding-one", Key: "authorization"}})
	require.NoError(t, err)
	require.Len(t, approved.Credentials, 1)
	_, err = service.Create(ctx, agent, "wrong-binding", "", nil, &api.SessionCredential{Origin: "https://mainloop.example.com", Header: "authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-mcp-binding-other", Key: "authorization"}})
	require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
	require.Equal(t, "approved", store.requestID)

	store.getResult = &api.Session{Id: created.Id, Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "kagent", Name: "other"}}
	_, err = service.Suspend(ctx, created.Id)
	require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
	store.getResult = &api.Session{Id: created.Id, Creator: "other", Agent: agent}
	_, err = service.Get(ctx, created.Id)
	require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
	// The fake deliberately returns records outside the DB creator filter.
	for i := 1; i <= 6; i++ {
		a := agent
		creator := "mainloop"
		if i == 1 || i == 3 {
			a = &api.ResourceReference{Namespace: "kagent", Name: "other"}
		}
		if i == 4 {
			creator = "other"
		}
		store.sessions = append(store.sessions, &api.Session{Id: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), Creator: creator, Agent: a})
	}
	page, err := service.List(ctx, ListRequest{PageSize: 2})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	require.Equal(t, store.sessions[1].Id, page.Sessions[0].Id)
	require.Equal(t, store.sessions[4].Id, page.Sessions[1].Id)
	page, err = service.List(ctx, ListRequest{PageSize: 2, PageToken: page.NextPageToken})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, store.sessions[5].Id, page.Sessions[0].Id)
	calls := store.listCalls
	_, err = service.List(ctx, ListRequest{AllCreators: true})
	require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
	require.Equal(t, calls, store.listCalls)
}
