package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type environmentTestStore struct {
	serviceTestStore
	existing  *apiv1alpha1.Session
	lookupErr error
	lookups   int
}

func (s *environmentTestStore) LookupSessionCreation(context.Context, string, string) (*apiv1alpha1.Session, error) {
	s.lookups++
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.existing == nil {
		return nil, database.ErrNotFound
	}
	return proto.CloneOf(s.existing), nil
}

type environmentTestPreparer struct {
	calls int
	err   error
}

func (p *environmentTestPreparer) Prepare(context.Context, *apiv1alpha1.ResourceReference, *apiv1alpha1.DevelopmentEnvironment) (string, *apiv1alpha1.RuntimeComposition, error) {
	p.calls++
	return strings.Repeat("c", 64), &apiv1alpha1.RuntimeComposition{PayloadImage: "registry/runtime@sha256:" + strings.Repeat("b", 64), Provider: "claude", Schema: 1}, p.err
}

var _ EnvironmentPreparer = (*environmentTestPreparer)(nil)

type environmentTestAuthorizer struct{ deny bool }

func (a environmentTestAuthorizer) CheckDevelopmentEnvironment(context.Context, *apiv1alpha1.ResourceReference, *apiv1alpha1.DevelopmentEnvironment) error {
	if a.deny {
		return errors.New("selection denied")
	}
	return nil
}

func (a environmentTestAuthorizer) Check(_ context.Context, _ auth.Principal, _ auth.Verb, r auth.Resource) error {
	if a.deny && r.Type == "DevelopmentEnvironment" {
		return errors.New("selection denied")
	}
	return nil
}

func TestSessionEnvironmentSelection(t *testing.T) {
	for _, name := range []string{"fresh", "legacy", "retry retains payload", "changed selection", "denied", "tag rejected", "missing catalog", "pending preparation", "lookup failed", "terminal preparation"} {
		t.Run(name, func(t *testing.T) {
			store := &environmentTestStore{}
			preparer := &environmentTestPreparer{}
			selection := &apiv1alpha1.DevelopmentEnvironment{Image: "registry/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "accepted-v1"}
			agent := &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "writer"}
			authorizer := environmentTestAuthorizer{deny: name == "denied"}
			options := []Option{WithEnvironmentPreparer(preparer), WithEnvironmentPolicy(authorizer)}
			switch name {
			case "legacy":
				selection = nil
			case "tag rejected":
				selection.Image = "registry/dev:latest"
			case "missing catalog":
				options = []Option{WithEnvironmentPolicy(authorizer)}
			case "terminal preparation":
				preparer.err = substrate.ErrGoldenSnapshotFailed
			case "pending preparation":
				preparer.err = errors.New("golden pending")
			case "lookup failed":
				store.lookupErr = errors.New("database unavailable")
			case "retry retains payload", "changed selection":
				store.existing = &apiv1alpha1.Session{Agent: agent, DevelopmentEnvironment: proto.CloneOf(selection), PreparedRevision: "stored-revision", RuntimeComposition: &apiv1alpha1.RuntimeComposition{PayloadImage: "original-payload", Provider: "claude", Schema: 1}}
				if name == "changed selection" {
					selection.PolicyIdentity = "different"
				}
			}
			service := NewService(store, authorizer, serviceTestWorkflow{}, options...)
			ctx := auth.AuthSessionTo(t.Context(), &authimpl.SimpleSession{P: auth.Principal{User: auth.User{ID: auth.MainloopService}, Service: auth.MainloopService}})
			created, err := service.CreateWithEnvironment(ctx, agent, "request-1", "", nil, selection)
			switch name {
			case "fresh":
				require.NoError(t, err)
				require.Equal(t, 1, preparer.calls)
				require.Equal(t, selection, created.DevelopmentEnvironment)
				require.Equal(t, strings.Repeat("c", 64), created.PreparedRevision)
				require.Contains(t, created.RuntimeComposition.PayloadImage, "@sha256:")
			case "legacy":
				require.NoError(t, err)
				require.Zero(t, preparer.calls)
				require.Nil(t, created.RuntimeComposition)
			case "retry retains payload":
				require.NoError(t, err)
				require.Zero(t, preparer.calls)
				require.Equal(t, "original-payload", created.RuntimeComposition.PayloadImage)
				require.Equal(t, "stored-revision", created.PreparedRevision)
			case "terminal preparation":
				require.True(t, serviceerrors.IsCode(err, serviceerrors.CodeFailedPrecondition))
				require.Nil(t, store.createInput)
			default:
				require.Error(t, err)
				require.Nil(t, store.createInput)
			}
		})
	}
}

func TestEnvironmentSelectionDefaultsToDenied(t *testing.T) {
	store := &environmentTestStore{}
	preparer := &environmentTestPreparer{}
	service := NewService(store, auth.NoopAuthorizer{}, serviceTestWorkflow{}, WithEnvironmentPreparer(preparer))
	ctx := auth.AuthSessionTo(t.Context(), &authimpl.SimpleSession{P: auth.Principal{User: auth.User{ID: auth.MainloopService}, Service: auth.MainloopService}})
	_, err := service.CreateWithEnvironment(ctx, &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "writer"}, "request", "", nil, &apiv1alpha1.DevelopmentEnvironment{Image: "registry/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "accepted-v1"})
	require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
	require.Zero(t, preparer.calls)
	require.Nil(t, store.createInput)
	require.Zero(t, store.lookups)
}

type environmentTestWorkflow struct {
	serviceTestWorkflow
	creates int
}

func (w *environmentTestWorkflow) Create(_ context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	w.creates++
	return session, nil
}

func TestEnvironmentSelectionRejectsWrongPrincipal(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "team-a", Agents: []string{"writer"}, DevelopmentEnvironmentRegistries: []string{"registry"}})
	require.NoError(t, err)
	policy = policy.WithRuntimePlatforms([]string{"linux/arm64"})
	for _, principal := range []auth.Principal{
		{User: auth.User{ID: "other"}, Service: auth.MainloopService},
		{User: auth.User{ID: auth.MainloopService}, Service: "other"},
		{User: auth.User{ID: auth.MainloopService}, Service: auth.MainloopService, Agent: auth.Agent{ID: "actor"}},
	} {
		t.Run(principal.User.ID+"/"+principal.Service+"/"+principal.Agent.ID, func(t *testing.T) {
			store := &environmentTestStore{}
			preparer := &environmentTestPreparer{}
			workflow := &environmentTestWorkflow{}
			service := NewService(store, auth.NoopAuthorizer{}, workflow, WithEnvironmentPreparer(preparer), WithEnvironmentPolicy(policy))
			ctx := auth.AuthSessionTo(t.Context(), &authimpl.SimpleSession{P: principal})
			_, err := service.CreateWithEnvironment(ctx, &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "writer"}, "request", "", nil, &apiv1alpha1.DevelopmentEnvironment{Image: "registry/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "accepted-v1"})
			require.True(t, serviceerrors.IsCode(err, serviceerrors.CodePermissionDenied))
			require.Zero(t, preparer.calls)
			require.Nil(t, store.createInput)
			require.Zero(t, store.lookups)
			require.Zero(t, workflow.creates)
		})
	}
}
