package session

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
)

var (
	// ErrEnvironmentNotReady means the Agent has no prepared revision to compose
	// yet. Retrying later can succeed.
	ErrEnvironmentNotReady = errors.New("agent revision is not ready for composition")
	// ErrEnvironmentUnsupported means the selection cannot be composed with the
	// Agent, its Harness or WorkerPool, or the payload catalog.
	ErrEnvironmentUnsupported = errors.New("development environment cannot be composed with this Agent")
	// ErrEnvironmentGoldenFailed means the composed golden snapshot failed to
	// build. A retry builds it again.
	ErrEnvironmentGoldenFailed = errors.New("development environment golden snapshot failed")
)

// EnvironmentPreparer prepares the composed variant of an Agent's latest
// revision outside store transactions and returns its revision ID once it can
// serve Sessions.
type EnvironmentPreparer interface {
	Prepare(context.Context, *apiv1.ResourceReference, *apiv1.DevelopmentEnvironment) (string, *apiv1.RuntimeComposition, error)
}

// WithEnvironmentPreparer enables development environment selection. Without
// it, a request that selects one fails.
func WithEnvironmentPreparer(preparer EnvironmentPreparer) Option {
	return func(service *Service) { service.environmentPreparer = preparer }
}

// prepareEnvironment admits the selection and resolves the composed revision the
// reservation pins. A retried request reuses the original preparation, so a
// catalog change after the first attempt does not change its Session.
func (s *Service) prepareEnvironment(ctx context.Context, agent *apiv1.ResourceReference, creator, requestID string, environment *apiv1.DevelopmentEnvironment) (string, *apiv1.RuntimeComposition, error) {
	if err := protovalidate.Validate(environment); err != nil {
		return "", nil, serviceerrors.NewInvalidArgument("development_environment is invalid", err)
	}
	if s.createPolicy == nil {
		return "", nil, serviceerrors.NewPermissionDenied("Development environment selection requires a session policy", nil)
	}
	if err := s.createPolicy.CheckDevelopmentEnvironment(ctx, agent, environment); err != nil {
		return "", nil, serviceerrors.NewPermissionDenied("Development environment selection is not permitted by the session policy", err)
	}
	if s.environmentPreparer == nil {
		return "", nil, serviceerrors.NewFailedPrecondition("Development environment composition is not configured", nil)
	}
	existing, err := s.store.GetSessionRequest(ctx, creator, requestID)
	switch {
	case err == nil:
		if !proto.Equal(existing.GetRuntimeComposition().GetDevelopmentEnvironment(), environment) {
			return "", nil, serviceerrors.NewAlreadyExists("request_id was already used for a different Session", database.ErrIdempotencyConflict)
		}
		return existing.GetPreparedRevision(), existing.GetRuntimeComposition(), nil
	case errors.Is(err, database.ErrIdempotencyConflict):
		return "", nil, serviceerrors.NewAlreadyExists("request_id belongs to a checkpoint fork", err)
	case !errors.Is(err, database.ErrNotFound):
		return "", nil, serviceerrors.NewInternal("Failed to read the Session request", err)
	}
	revision, composition, err := s.environmentPreparer.Prepare(ctx, agent, environment)
	switch {
	case err == nil:
		return revision, composition, nil
	case errors.Is(err, ErrEnvironmentUnsupported):
		return "", nil, serviceerrors.NewFailedPrecondition("Development environment cannot be composed with this Agent", err)
	case errors.Is(err, ErrEnvironmentGoldenFailed):
		return "", nil, serviceerrors.NewFailedPrecondition("Development environment golden preparation failed; retry or select corrected inputs", err)
	case errors.Is(err, ErrEnvironmentNotReady):
		return "", nil, serviceerrors.NewUnavailable("Agent revision is not ready for composition", err)
	default:
		return "", nil, serviceerrors.NewUnavailable("Failed to prepare the development environment", err)
	}
}
