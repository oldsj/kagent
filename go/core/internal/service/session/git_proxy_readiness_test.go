package session

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type deniedProxyResume struct{ *lifecycleTestActors }

func (a deniedProxyResume) ResumeActor(context.Context, string, string) (*ateapipb.Actor, error) {
	return nil, status.Error(codes.Unavailable, "controlled warmup failure")
}
func TestGitProxyFailedWarmupRetainsIntentAndReferences(t *testing.T) {
	store, _ := lifecycleFixture(t)
	revision := *store.revision
	revision.Revision = "proxy-held-revision"
	revision.ActorTemplateName = "proxy-held-template"
	revision.ActorTemplateUID = "proxy-held-template-uid"
	revision.GitOrigins = []string{"github.com"}
	revision.EgressDestinations = []string{workspace.ReadProxyOrigin + ":80"}
	store.revision = &revision
	require.NoError(t, store.UpsertAgentDefinition(t.Context(), database.AgentDefinition{Namespace: revision.Namespace, AgentName: revision.AgentName, AgentUID: revision.AgentUID, DesiredRevision: revision.Revision}))
	require.NoError(t, store.RecordRuntimeRevision(t.Context(), revision, true))
	actors := deniedProxyResume{&lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}}
	template, err := actors.GetActorTemplate(t.Context(), revision.ActorTemplateAtespace, revision.ActorTemplateName)
	require.NoError(t, err)
	actors.template = template
	actors.template.Metadata.Uid = revision.ActorTemplateUID
	service := NewService(store, serviceTestAuthorizer{}, NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083"))
	refs := []*api.SessionCredential{{Origin: workspace.ReadProxyOrigin, Header: "Authorization", SecretRef: &api.SecretKeyReference{Name: "mainloop-git-read-unpublished", Key: "authorization"}}}
	ctx := serviceTestContext("alice")
	created, err := service.Create(ctx, &api.ResourceReference{Namespace: revision.Namespace, Name: revision.AgentName}, "held-reference", "", &api.Workspace{Repo: "https://github.com/o/r"}, refs...)
	require.NoError(t, err)
	generation, err := store.GetRuntimeGeneration(ctx, created.Id)
	require.NoError(t, err)
	_, err = service.Suspend(ctx, created.Id)
	require.NoError(t, err)
	_, err = service.Resume(ctx, created.Id)
	require.Error(t, err)
	held, err := service.Get(ctx, created.Id)
	require.NoError(t, err)
	require.Nil(t, held.RuntimeAssociation)
	require.Equal(t, api.RuntimeOperation_RUNTIME_OPERATION_RESUME, held.Operation)
	require.True(t, proto.Equal(refs[0], held.Credentials[0]))
	after, err := store.GetRuntimeGeneration(ctx, created.Id)
	require.NoError(t, err)
	require.Equal(t, generation.ID, after.ID)
	require.Equal(t, generation.ActorUID, after.ActorUID)
}
