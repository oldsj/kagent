package database

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestEnvironmentRequestIdentityAndPinnedRevision(t *testing.T) {
	request := &apiv1alpha1.Session{Agent: &apiv1alpha1.ResourceReference{Namespace: "agents", Name: "writer"}, DevelopmentEnvironment: &apiv1alpha1.DevelopmentEnvironment{Image: "D", Platform: "linux/arm64", PolicyIdentity: "v1"}, RuntimeComposition: &apiv1alpha1.RuntimeComposition{PayloadImage: "R", Provider: "claude", Schema: 1}}
	same := proto.CloneOf(request)
	require.True(t, sameSessionRequest(same, request))
	same.Name = "renamed"
	require.True(t, sameSessionRequest(same, request))
	for _, field := range []string{"image", "platform", "policy", "payload", "provider", "schema"} {
		changed := proto.CloneOf(request)
		switch field {
		case "image":
			changed.DevelopmentEnvironment.Image = "new-D"
		case "platform":
			changed.DevelopmentEnvironment.Platform = "linux/amd64"
		case "policy":
			changed.DevelopmentEnvironment.PolicyIdentity = "v2"
		case "payload":
			changed.RuntimeComposition.PayloadImage = "new-R"
		case "provider":
			changed.RuntimeComposition.Provider = "codex"
		case "schema":
			changed.RuntimeComposition.Schema = 2
		}
		require.False(t, sameSessionRequest(changed, request), field)
	}
	snapshot, err := json.Marshal(EnvironmentRevisionSnapshot{BaseRevision: "base", Environment: request.DevelopmentEnvironment, Composition: request.RuntimeComposition})
	require.NoError(t, err)
	pinned := runtimeRevisionRow{Namespace: "agents", AgentName: "writer", SourceSnapshot: snapshot}
	require.NoError(t, validateEnvironmentRevision(pinned, request, "base"))
	require.ErrorIs(t, validateEnvironmentRevision(pinned, request, "new-base"), ErrConflict)
	request.DevelopmentEnvironment.Image = "other"
	require.ErrorIs(t, validateEnvironmentRevision(pinned, request, "base"), ErrConflict)
	pinned.SourceSnapshot = []byte(`{"environment":null}`)
	require.Error(t, validateEnvironmentRevision(pinned, request, "base"))
}

func TestComposedSessionPersistenceAndRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	client := NewClient(setupTestDB(t))
	ctx := t.Context()
	sessionFixture(t, client, ctx, "team-a", "base", "writer", "claude")
	base, err := client.GetRuntimeRevision(ctx, "base")
	require.NoError(t, err)
	request := newSessionRequest(uuid.NewString(), "writer", "claude", "")
	request.DevelopmentEnvironment = &apiv1alpha1.DevelopmentEnvironment{Image: "D", Platform: "linux/arm64", PolicyIdentity: "v1"}
	request.RuntimeComposition = &apiv1alpha1.RuntimeComposition{PayloadImage: "R", Provider: "claude", Schema: 1}
	request.PreparedRevision = "composed"
	variant := *base
	variant.Revision = "composed"
	variant.ActorTemplateName = "composed-template"
	variant.ActorTemplateUID = "composed-uid"
	variant.SourceSnapshot, err = json.Marshal(EnvironmentRevisionSnapshot{BaseRevision: "base", Environment: request.DevelopmentEnvironment, Composition: request.RuntimeComposition})
	require.NoError(t, err)
	require.NoError(t, client.RecordRuntimeRevision(ctx, variant, true))
	unreferenced, err := client.ListUnreferencedRuntimeRevisions(ctx)
	require.NoError(t, err)
	require.Empty(t, unreferenced, "active base protects variant before first reservation")
	deleting, err := client.BeginRuntimeRevisionDeletion(ctx, "composed")
	require.NoError(t, err)
	require.Nil(t, deleting)
	session, created, err := client.CreateSession(ctx, request, "selected-request")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "composed", session.PreparedRevision)
	read, err := client.GetSession(ctx, session.Id, "alice")
	require.NoError(t, err)
	require.True(t, proto.Equal(request.RuntimeComposition, read.RuntimeComposition))
	retry, created, err := client.CreateSession(ctx, request, "selected-request")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, session.Id, retry.Id)
	altered := proto.CloneOf(request)
	altered.RuntimeComposition.PayloadImage = "different"
	_, _, err = client.CreateSession(ctx, altered, "selected-request")
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	legacy, _, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "writer", "claude", ""), "legacy-request")
	require.NoError(t, err)
	require.Equal(t, "base", legacy.PreparedRevision, "variant must not promote shared latest-successful pointer")
	require.NoError(t, client.RetireAgentIdentities(ctx, "team-a", "writer", nil))
	unreferenced, err = client.ListUnreferencedRuntimeRevisions(ctx)
	require.NoError(t, err)
	require.Empty(t, unreferenced, "both Session pins survive Agent retirement")
}

func TestForkInheritsExactEnvironmentComposition(t *testing.T) {
	session := &apiv1alpha1.Session{}
	snapshot := EnvironmentRevisionSnapshot{BaseRevision: "base", Environment: &apiv1alpha1.DevelopmentEnvironment{Image: "D"}, Composition: &apiv1alpha1.RuntimeComposition{PayloadImage: "R"}}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, inheritEnvironmentRevision(raw, session))
	require.True(t, proto.Equal(snapshot.Environment, session.DevelopmentEnvironment))
	require.True(t, proto.Equal(snapshot.Composition, session.RuntimeComposition))
	require.NoError(t, inheritEnvironmentRevision([]byte(`[]`), &apiv1alpha1.Session{}))
	require.NoError(t, inheritEnvironmentRevision([]byte(`{}`), &apiv1alpha1.Session{}))
	require.Error(t, inheritEnvironmentRevision([]byte(`{"baseRevision":"base"}`), &apiv1alpha1.Session{}))
}
