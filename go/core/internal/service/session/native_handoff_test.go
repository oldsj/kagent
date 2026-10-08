package session

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type nativeTestStore struct {
	*serviceTestStore
	receipt  *api.WorkspacePreparationReceipt
	prepares int
}

func (s *nativeTestStore) GetWorkspacePreparationReceipt(context.Context, string) (*api.WorkspacePreparationReceipt, error) {
	return proto.CloneOf(s.receipt), nil
}
func (s *nativeTestStore) BeginWorkspacePreparation(context.Context, *api.PrepareSessionWorkspaceRequest, *api.NativeWorkspacePreparation, *api.Session) (*api.WorkspacePreparationReceipt, error) {
	s.prepares++
	return nil, nil
}

type preparationPrincipal struct{ principal auth.Principal }

func (p preparationPrincipal) Principal() auth.Principal { return p.principal }

func nativeServiceInput(t *testing.T) *api.PrepareSessionWorkspaceRequest {
	t.Helper()
	digest, err := workspace.SetupDigest("child")
	require.NoError(t, err)
	return &api.PrepareSessionWorkspaceRequest{SessionId: uuid.NewString(), ActionId: "create:prepare", CreateRequestId: "create", GenerationId: uuid.NewString(), ActorUid: "owned", PreparedRevision: "original", Workspace: &api.Workspace{Repo: "https://github.com/owner/repo.git", Ref: strings.Repeat("a", 40), Branch: "feature"}, DevelopmentEnvironment: &api.DevelopmentEnvironment{Image: "fixture.test/d@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64", PolicyIdentity: "original"}, RuntimeComposition: &api.RuntimeComposition{PayloadImage: "fixture.test/r@sha256:" + strings.Repeat("b", 64), Provider: "codex", Schema: 1, CliVersion: "1.0"}, SetupProfile: "child", SetupDigest: digest}
}

func TestNativePreparationServiceRequiresExactServicePrincipal(t *testing.T) {
	for _, principal := range []auth.Principal{{User: auth.User{ID: "mainloop"}}, {Service: "other", User: auth.User{ID: "mainloop"}}, {Service: "mainloop", User: auth.User{ID: "other"}}, {Service: "mainloop", User: auth.User{ID: "mainloop"}, Agent: auth.Agent{ID: "agent"}}} {
		store := &nativeTestStore{serviceTestStore: &serviceTestStore{}}
		service := NewService(store, &auth.NoopAuthorizer{}, serviceTestWorkflow{})
		_, err := service.PrepareWorkspace(auth.AuthSessionTo(t.Context(), preparationPrincipal{principal}), nativeServiceInput(t))
		require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
		require.Zero(t, store.prepares)
	}
}

func TestNativePreparationServiceRejectsIntrinsicAndForeignInputs(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	for _, name := range []string{"unknown", "oversize", "setup", "creator", "Agent", "namespace", "share"} {
		t.Run(name, func(t *testing.T) {
			input := nativeServiceInput(t)
			session := &api.Session{Id: input.SessionId, Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}, Workspace: input.Workspace, DevelopmentEnvironment: input.DevelopmentEnvironment}
			store := &nativeTestStore{serviceTestStore: &serviceTestStore{getResult: session}}
			service := NewService(store, policy, serviceTestWorkflow{})
			ctx := auth.AuthSessionTo(t.Context(), mainloopSession{})
			switch name {
			case "unknown":
				input.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
			case "oversize":
				input.RuntimeComposition.CliVersion = strings.Repeat("x", 8193)
			case "setup":
				input.SetupDigest = strings.Repeat("0", 64)
			case "creator":
				session.Creator = "other"
			case "Agent":
				session.Agent.Name = "other"
			case "namespace":
				session.Agent.Namespace = "other"
			case "share":
				ctx = auth.ShareContextTo(ctx, &auth.ShareContext{SessionID: session.Id, UserID: "mainloop", ReadOnly: false})
			}
			_, err := service.PrepareWorkspace(ctx, input)
			require.Error(t, err)
			require.Zero(t, store.prepares)
		})
	}
}

func TestNativePreparationReceiptOnlyOnAuthorizedGet(t *testing.T) {
	session := &api.Session{Id: uuid.NewString(), Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}}
	cached := &api.WorkspacePreparationReceipt{Classification: "confirmed"}
	session.WorkspacePreparation = cached
	store := &nativeTestStore{serviceTestStore: &serviceTestStore{getResult: session, sessions: []*api.Session{session}}, receipt: &api.WorkspacePreparationReceipt{Classification: "uncertain"}}
	service := NewService(store, &auth.NoopAuthorizer{}, serviceTestWorkflow{})
	got, err := service.Get(serviceTestContext("mainloop"), session.Id)
	require.NoError(t, err)
	require.Equal(t, "uncertain", got.WorkspacePreparation.Classification)
	page, err := service.List(serviceTestContext("mainloop"), ListRequest{})
	require.NoError(t, err)
	require.Nil(t, page.Sessions[0].WorkspacePreparation)
	resumed, err := service.Resume(serviceTestContext("mainloop"), session.Id)
	require.NoError(t, err)
	require.Nil(t, resumed.WorkspacePreparation)
	require.Same(t, cached, session.WorkspacePreparation)
}
