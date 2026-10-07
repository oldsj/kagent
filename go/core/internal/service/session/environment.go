package session

import (
	"context"
	"errors"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"google.golang.org/protobuf/proto"
)

// EnvironmentPreparer prepares an immutable revision outside store transactions.
type EnvironmentPreparer interface {
	Prepare(context.Context, *apiv1alpha1.ResourceReference, *apiv1alpha1.DevelopmentEnvironment) (string, *apiv1alpha1.RuntimeComposition, error)
}

// EnvironmentPolicy is an explicit trusted selection boundary. Ordinary Session
// authorizers, including the OSS no-op authorizer, do not enable composition.
type EnvironmentPolicy interface {
	CheckDevelopmentEnvironment(context.Context, *apiv1alpha1.ResourceReference, *apiv1alpha1.DevelopmentEnvironment) error
}

func WithEnvironmentPolicy(policy EnvironmentPolicy) Option {
	return func(service *Service) { service.environmentPolicy = policy }
}

type environmentCreationStore interface {
	LookupSessionCreation(context.Context, string, string) (*apiv1alpha1.Session, error)
}

// WithEnvironmentPreparer enables operator-configured environment composition.
func WithEnvironmentPreparer(preparer EnvironmentPreparer) Option {
	return func(service *Service) { service.environmentPreparer = preparer }
}

// CreateWithEnvironment is the typed controller selection boundary. Omitting
// selection calls the exact legacy creation path, including request identity.
func (s *Service) CreateWithEnvironment(ctx context.Context, agent *apiv1alpha1.ResourceReference, requestID, name string, requested *apiv1alpha1.Workspace, environment *apiv1alpha1.DevelopmentEnvironment, credentials ...*apiv1alpha1.SessionCredential) (*apiv1alpha1.Session, error) {
	return s.create(ctx, agent, requestID, name, requested, environment, credentials...)
}

func (s *Service) prepareEnvironment(ctx context.Context, request *apiv1alpha1.Session, requestID string) error {
	principal, _ := auth.AuthSessionFrom(ctx)
	if s.environmentPolicy == nil || principal == nil || principal.Principal().Service != auth.MainloopService {
		return serviceerrors.NewPermissionDenied("Development environment selection requires trusted service authentication", nil)
	}
	if err := s.environmentPolicy.CheckDevelopmentEnvironment(ctx, request.GetAgent(), request.GetDevelopmentEnvironment()); err != nil {
		return serviceerrors.NewPermissionDenied("Development environment selection is not authorized", err)
	}
	if err := s.authorizer.Check(ctx, principal.Principal(), auth.VerbCreate, auth.Resource{Type: "DevelopmentEnvironment", Namespace: request.GetAgent().GetNamespace(), Name: request.GetDevelopmentEnvironment().GetImage()}); err != nil {
		return serviceerrors.NewPermissionDenied("Not authorized to select a development environment", err)
	}
	if s.environmentPreparer == nil {
		return serviceerrors.NewFailedPrecondition("Development environment composition is not configured", nil)
	}
	store, ok := s.store.(environmentCreationStore)
	if !ok {
		return serviceerrors.NewInternal("Environment creation store is not configured", nil)
	}
	existing, err := store.LookupSessionCreation(ctx, request.GetCreator(), requestID)
	if err == nil {
		if !database.SameSessionSelection(existing, request) {
			return serviceerrors.NewAlreadyExists("request_id was already used for a different Session", database.ErrIdempotencyConflict)
		}
		if existing.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED {
			return serviceerrors.NewFailedPrecondition("request_id belongs to a deleted Session", database.ErrFailedPrecondition)
		}
		request.PreparedRevision = existing.GetPreparedRevision()
		request.RuntimeComposition = proto.CloneOf(existing.GetRuntimeComposition())
		return nil
	}
	if errors.Is(err, database.ErrIdempotencyConflict) {
		return serviceerrors.NewAlreadyExists("request_id belongs to a checkpoint fork", err)
	}
	if !errors.Is(err, database.ErrNotFound) {
		return serviceerrors.NewInternal("Failed to read environment creation request", err)
	}
	revision, composition, err := s.environmentPreparer.Prepare(ctx, request.GetAgent(), request.GetDevelopmentEnvironment())
	if err != nil {
		if errors.Is(err, substrate.ErrGoldenSnapshotFailed) {
			return serviceerrors.NewFailedPrecondition("Development environment golden preparation failed; select corrected inputs", err)
		}
		return serviceerrors.NewUnavailable("Failed to prepare development environment revision", err)
	}
	request.PreparedRevision, request.RuntimeComposition = revision, composition
	return nil
}
