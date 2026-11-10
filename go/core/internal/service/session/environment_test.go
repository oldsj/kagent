package session

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
)

func TestServiceCreateWithEnvironment(t *testing.T) {
	agent := &apiv1.ResourceReference{Namespace: "team-a", Name: "assistant"}
	selected := &apiv1.DevelopmentEnvironment{
		Image:          "registry.example/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Platform:       "linux/arm64",
		PolicyIdentity: "project/v1",
	}
	prepared := &apiv1.RuntimeComposition{PayloadImage: "payload", Provider: "claude", Schema: 1, CliVersion: "2.1.285"}

	t.Run("Given an admitting policy and a preparer, when a Session selects a development environment, assert that the reservation pins the prepared variant", func(t *testing.T) {
		store := &environmentTestStore{serviceTestStore: &serviceTestStore{}}
		preparer := &fakePreparer{revision: "variant", composition: prepared}
		service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{},
			WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(preparer))

		_, err := service.CreateWithEnvironment(serviceTestContext("alice"), agent, "request-1", "", nil, selected)

		if err != nil {
			t.Fatalf("CreateWithEnvironment() error = %v", err)
		}
		if store.createInput.GetPreparedRevision() != "variant" || !proto.Equal(store.createInput.GetRuntimeComposition(), prepared) || !proto.Equal(store.createInput.GetDevelopmentEnvironment(), selected) {
			t.Fatalf("reserved %v, want the prepared variant and composition", store.createInput)
		}
		if preparer.calls != 1 {
			t.Fatalf("preparer calls = %d, want 1", preparer.calls)
		}
	})

	t.Run("Given a Session already reserved for the request ID, when the same selection is retried, assert that the original preparation is reused", func(t *testing.T) {
		store := &environmentTestStore{serviceTestStore: &serviceTestStore{}, existing: &apiv1.Session{PreparedRevision: "original", DevelopmentEnvironment: selected, RuntimeComposition: prepared}}
		preparer := &fakePreparer{revision: "newer", composition: prepared}
		service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{},
			WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(preparer))

		_, err := service.CreateWithEnvironment(serviceTestContext("alice"), agent, "request-1", "", nil, selected)

		if err != nil {
			t.Fatalf("CreateWithEnvironment() error = %v", err)
		}
		if preparer.calls != 0 || store.createInput.GetPreparedRevision() != "original" {
			t.Fatalf("preparer calls = %d, reserved revision %q; want the original preparation", preparer.calls, store.createInput.GetPreparedRevision())
		}
	})

	t.Run("Given a Session reserved for the request ID with another image, when a different selection reuses the ID, assert that it conflicts before preparing", func(t *testing.T) {
		other := proto.CloneOf(selected)
		other.PolicyIdentity = "project/v2"
		store := &environmentTestStore{serviceTestStore: &serviceTestStore{}, existing: &apiv1.Session{PreparedRevision: "original", DevelopmentEnvironment: other, RuntimeComposition: prepared}}
		preparer := &fakePreparer{revision: "variant", composition: prepared}
		service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{},
			WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(preparer))

		_, err := service.CreateWithEnvironment(serviceTestContext("alice"), agent, "request-1", "", nil, selected)

		if !serviceerrors.IsCode(err, serviceerrors.CodeAlreadyExists) || preparer.calls != 0 || store.createInput != nil {
			t.Fatalf("error = %v, preparer calls = %d, reserved = %v; want AlreadyExists before any work", err, preparer.calls, store.createInput)
		}
	})

	refusals := []struct {
		name    string
		options []Option
		code    serviceerrors.Code
	}{
		{"no session policy", []Option{WithEnvironmentPreparer(&fakePreparer{})}, serviceerrors.CodePermissionDenied},
		{"a policy that denies the registry", []Option{WithCreatePolicy(environmentPolicy{err: errors.New("registry")}), WithEnvironmentPreparer(&fakePreparer{})}, serviceerrors.CodePermissionDenied},
		{"no preparer", []Option{WithCreatePolicy(environmentPolicy{})}, serviceerrors.CodeFailedPrecondition},
		{"a preparer that cannot compose the Agent", []Option{WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(&fakePreparer{err: ErrEnvironmentUnsupported})}, serviceerrors.CodeFailedPrecondition},
		{"a failed golden build", []Option{WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(&fakePreparer{err: ErrEnvironmentGoldenFailed})}, serviceerrors.CodeFailedPrecondition},
		{"an Agent revision still preparing", []Option{WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(&fakePreparer{err: ErrEnvironmentNotReady})}, serviceerrors.CodeUnavailable},
	}
	for _, refusal := range refusals {
		t.Run("Given "+refusal.name+", when a Session selects a development environment, assert that nothing is reserved", func(t *testing.T) {
			store := &environmentTestStore{serviceTestStore: &serviceTestStore{}}
			service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{}, refusal.options...)

			_, err := service.CreateWithEnvironment(serviceTestContext("alice"), agent, "request-1", "", nil, selected)

			if !serviceerrors.IsCode(err, refusal.code) || store.createInput != nil {
				t.Fatalf("error = %v, reserved = %v; want code %v and no reservation", err, store.createInput, refusal.code)
			}
		})
	}

	t.Run("Given an Agent revision that changed after preparation, when the reservation finds the variant stale, assert that the caller is asked to retry", func(t *testing.T) {
		store := &environmentTestStore{serviceTestStore: &serviceTestStore{createErr: database.ErrConflict}}
		service := NewService(store, serviceTestAuthorizer{}, serviceTestWorkflow{},
			WithCreatePolicy(environmentPolicy{}), WithEnvironmentPreparer(&fakePreparer{revision: "variant", composition: prepared}))

		_, err := service.CreateWithEnvironment(serviceTestContext("alice"), agent, "request-1", "", nil, selected)

		if !serviceerrors.IsCode(err, serviceerrors.CodeAborted) {
			t.Fatalf("error = %v, want Aborted", err)
		}
	})
}

type environmentTestStore struct {
	*serviceTestStore
	existing *apiv1.Session
}

func (s *environmentTestStore) GetSessionRequest(context.Context, string, string) (*apiv1.Session, error) {
	if s.existing == nil {
		return nil, database.ErrNotFound
	}
	return s.existing, nil
}

type environmentPolicy struct{ err error }

func (environmentPolicy) CheckCreateSession(context.Context, *apiv1.ResourceReference, []*apiv1.SessionCredential) error {
	return nil
}

func (p environmentPolicy) CheckDevelopmentEnvironment(context.Context, *apiv1.ResourceReference, *apiv1.DevelopmentEnvironment) error {
	return p.err
}

type fakePreparer struct {
	revision    string
	composition *apiv1.RuntimeComposition
	err         error
	calls       int
}

func (p *fakePreparer) Prepare(context.Context, *apiv1.ResourceReference, *apiv1.DevelopmentEnvironment) (string, *apiv1.RuntimeComposition, error) {
	p.calls++
	return p.revision, p.composition, p.err
}
