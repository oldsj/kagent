package taskstore

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

type dotfilesStore struct {
	Store
	session *apiv1.Session
}

func (s *dotfilesStore) GetSessionForRuntime(_ context.Context, id, _ string) (*apiv1.Session, error) {
	session := s.session
	session.Id, session.A2AAuthority = id, substrate.ActorHost("team-a", substrate.ActorName(id), "")
	return session, nil
}

func (s *dotfilesStore) IssueWorkspacePreparation(context.Context, *apiv1.Session, string) (*database.RuntimePreparation, error) {
	return &database.RuntimePreparation{}, nil
}

// The runtime learns where to fetch the bundle and which header to send with a
// placeholder; the credential's value stays with the gateway.
func TestGetWorkspaceDeliversTheDotfilesWithTheirCredentialHeader(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	store := &dotfilesStore{session: &apiv1.Session{
		Credentials: []*apiv1.SessionCredential{
			{Origin: "https://api.example.test", Header: "authorization", SecretRef: &apiv1.SecretKeyReference{Name: "s", Key: "api"}},
			{Origin: "https://mcp.example.test", Header: "x-mcp-token", SecretRef: &apiv1.SecretKeyReference{Name: "s", Key: "mcp"}},
		},
		Dotfiles: &apiv1.Dotfiles{Url: "https://mcp.example.test/dotfiles/" + digest, Digest: digest},
	}}
	response, err := NewService(store, nil, nil, 0).GetWorkspace(runtimeContext(t), &runtimev1.TaskStoreServiceGetWorkspaceRequest{SessionId: "00000000-0000-0000-0000-000000000001"})
	require.NoError(t, err)
	got := response.GetDotfiles()
	require.NotNil(t, got)
	require.Equal(t, "https://mcp.example.test/dotfiles/"+digest, got.GetUrl())
	require.Equal(t, digest, got.GetDigest())
	require.Equal(t, "x-mcp-token", got.GetHeader())
}

func TestGetWorkspaceWithoutDotfilesDeliversNone(t *testing.T) {
	store := &dotfilesStore{session: &apiv1.Session{}}
	response, err := NewService(store, nil, nil, 0).GetWorkspace(runtimeContext(t), &runtimev1.TaskStoreServiceGetWorkspaceRequest{SessionId: "00000000-0000-0000-0000-000000000001"})
	require.NoError(t, err)
	require.Nil(t, response.GetDotfiles())
}
