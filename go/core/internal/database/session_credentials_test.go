package database

import (
	"testing"

	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSessionCredentialPersistenceAndRetry(t *testing.T) {
	c := NewClient(setupTestDB(t))
	ctx := t.Context()
	sessionFixture(t, c, ctx, "team-a", "revision-1", "assistant", "kagent")
	// Fixture replacement deliberately uses a new revision, since inputs are immutable.
	original, err := c.GetRuntimeRevision(ctx, "revision-1")
	require.NoError(t, err)
	original.Revision = "revision-credentials"
	original.ActorTemplateName = "session-credential-template"
	original.EgressDestinations = []string{"http://echo.test.svc.cluster.local:80", "https://api.example.com:443"}
	original.Credentials = []egress.Credential{{Hostname: "api.example.com", Header: "authorization", URI: "ate-secret://k8s.io/default/team-a/model/key"}}
	require.NoError(t, c.UpsertAgentDefinition(ctx, AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: original.AgentUID, DesiredRevision: original.Revision}))
	require.NoError(t, c.RecordRuntimeRevision(ctx, *original, true))
	request := newSessionRequest(uuid.NewString(), "assistant", "kagent", "")
	request.Credentials = []*apiv1alpha1.SessionCredential{{Origin: "http://echo.test.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: "one"}}}
	saved, created, err := c.CreateSession(ctx, request, "credentials")
	require.NoError(t, err)
	require.True(t, created)
	read, err := c.GetSessionByID(ctx, saved.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(request.Credentials[0], read.Credentials[0]))
	retry, created, err := c.CreateSession(ctx, request, "credentials")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, saved.Id, retry.Id)
	changed := proto.CloneOf(request)
	changed.Credentials[0].SecretRef.Key = "two"
	_, _, err = c.CreateSession(ctx, changed, "credentials")
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	for _, origin := range []string{"http://unlisted.test.svc.cluster.local", "https://api.example.com"} {
		bad := proto.CloneOf(request)
		bad.Id = uuid.NewString()
		bad.Credentials[0].Origin = origin
		_, _, err = c.CreateSession(ctx, bad, uuid.NewString())
		require.ErrorIs(t, err, ErrSessionCredentialNotAllowed)
	}
}
