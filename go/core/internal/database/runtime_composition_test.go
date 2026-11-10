package database

import (
	"context"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

func TestComposedSessions(t *testing.T) {
	composition := func(identity string) *RevisionComposition {
		return &RevisionComposition{
			Environment: &apiv1.DevelopmentEnvironment{
				Image:    "registry.example/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Platform: "linux/arm64", PolicyIdentity: identity,
			},
			Runtime: &apiv1.RuntimeComposition{
				PayloadImage: "registry.example/runtime-claude@sha256:2222222222222222222222222222222222222222222222222222222222222222",
				Provider:     "claude", Schema: 1, CliVersion: "2.1.285",
			},
		}
	}
	recordVariant := func(t *testing.T, client *Client, ctx context.Context, variant, base string, selected *RevisionComposition) {
		t.Helper()
		require.NoError(t, recordErr(client.RecordRuntimeRevision(ctx, RuntimeRevision{
			Revision: variant, Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid",
			SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, EgressDestinations: []string{},
			ActorTemplateAtespace: "team-a", ActorTemplateName: variant + "-actor-template", ActorTemplateUID: variant + "-actor-uid",
			BaseRevision: base, Composition: selected,
		}, RecordRevisionOptions{Ready: true})))
	}
	composedRequest := func(variant string, selected *RevisionComposition) *apiv1.Session {
		request := newSessionRequest(uuid.NewString(), "assistant", "claude", "")
		request.PreparedRevision, request.DevelopmentEnvironment, request.RuntimeComposition = variant, selected.Environment, selected.Runtime
		return request
	}

	t.Run("Given a variant prepared from the Agent's latest revision, when a Session pins it, assert that the Session reports its composition and later reads find the request", func(t *testing.T) {
		client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
		sessionFixture(t, client, ctx, "team-a", "base", "assistant", "claude")
		recordVariant(t, client, ctx, "variant", "base", composition("project/v1"))

		created, _, err := client.CreateSession(ctx, composedRequest("variant", composition("project/v1")), "request-1")

		require.NoError(t, err)
		require.Equal(t, "variant", created.GetPreparedRevision())
		require.True(t, proto.Equal(composition("project/v1").Environment, created.GetDevelopmentEnvironment()), "got %v", created.GetDevelopmentEnvironment())
		require.True(t, proto.Equal(composition("project/v1").Runtime, created.GetRuntimeComposition()), "got %v", created.GetRuntimeComposition())
		found, err := client.GetSessionRequest(ctx, "alice", "request-1")
		require.NoError(t, err)
		require.Equal(t, created.GetId(), found.GetId())
	})

	t.Run("Given a variant of a revision the Agent has moved past, when a Session pins it, assert that the reservation conflicts", func(t *testing.T) {
		client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
		sessionFixture(t, client, ctx, "team-a", "old-base", "assistant", "claude")
		recordVariant(t, client, ctx, "old-variant", "old-base", composition("project/v1"))
		sessionFixture(t, client, ctx, "team-a", "new-base", "assistant", "claude")

		_, _, err := client.CreateSession(ctx, composedRequest("old-variant", composition("project/v1")), "request-1")

		require.ErrorIs(t, err, ErrConflict)
	})

	t.Run("Given a variant recorded for another selection, when a Session pins it, assert that the reservation conflicts", func(t *testing.T) {
		client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
		sessionFixture(t, client, ctx, "team-a", "base", "assistant", "claude")
		recordVariant(t, client, ctx, "variant", "base", composition("project/v1"))

		_, _, err := client.CreateSession(ctx, composedRequest("variant", composition("project/v2")), "request-1")

		require.ErrorIs(t, err, ErrConflict)
	})

	t.Run("Given an unpinned variant of the Agent's latest revision, when garbage collection lists revisions, assert that the variant is retained until the base is superseded", func(t *testing.T) {
		client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
		sessionFixture(t, client, ctx, "team-a", "base", "assistant", "claude")
		recordVariant(t, client, ctx, "variant", "base", composition("project/v1"))

		retained := unreferencedRevisions(t, client, ctx)
		sessionFixture(t, client, ctx, "team-a", "next", "assistant", "claude")
		released := unreferencedRevisions(t, client, ctx)

		require.NotContains(t, retained, "variant")
		require.Contains(t, released, "variant")
	})

	t.Run("Given a revision recorded with a composition but no base, when it is recorded, assert that it is rejected", func(t *testing.T) {
		client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
		sessionFixture(t, client, ctx, "team-a", "base", "assistant", "claude")

		_, err := client.RecordRuntimeRevision(ctx, RuntimeRevision{
			Revision: "variant", Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid",
			SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, EgressDestinations: []string{},
			ActorTemplateAtespace: "team-a", ActorTemplateName: "variant-actor-template", ActorTemplateUID: "variant-actor-uid",
			Composition: composition("project/v1"),
		}, RecordRevisionOptions{Ready: true})

		require.Error(t, err)
	})
}

func unreferencedRevisions(t *testing.T, client *Client, ctx context.Context) []string {
	t.Helper()
	artifacts, err := client.ListUnreferencedRuntimeRevisions(ctx)
	require.NoError(t, err)
	revisions := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		revisions = append(revisions, artifact.Revision)
	}
	return revisions
}
