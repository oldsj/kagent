package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// staleRuntimeMessage is the one refusal callers act on by re-reading the Session
// and retrying. Its wording is the 0.4 contract Mainloop matches on.
const staleRuntimeMessage = "Current prepared runtime identity is unavailable"

type preparationStore interface {
	GetRevisionConfig(context.Context, string) ([]byte, error)
	BeginWorkspacePreparation(context.Context, *apiv1.PrepareSessionWorkspaceRequest, *runtimev1.WorkspacePreparation, *apiv1.Session) (*apiv1.WorkspacePreparationReceipt, error)
	GetWorkspacePreparationReceipt(context.Context, string) (*apiv1.WorkspacePreparationReceipt, error)
}

type runtimeWaker interface {
	WakeRuntime(context.Context, *apiv1.Session) error
}

// preparationAssignment derives the runtime's assignment from the request, the
// Session, its observed runtime and its revision's compiled config. It refuses a Session
// whose Git policy does not enforce both trusted proxies or whose credentials do
// not name exactly one read and one push proxy credential.
func preparationAssignment(request *apiv1.PrepareSessionWorkspaceRequest, session *apiv1.Session, association *apiv1.RuntimeAssociation, config []byte) (*runtimev1.WorkspacePreparation, error) {
	var compiled struct {
		Git *apiworkspace.Git `json:"git"`
	}
	if err := json.Unmarshal(config, &compiled); err != nil {
		return nil, fmt.Errorf("decode compiled config: %w", err)
	}
	policy := compiled.Git
	if policy == nil || policy.ReadProxyOrigin == nil || policy.PushProxyOrigin == nil || policy.Validate() != nil {
		return nil, errors.New("compiled Git policy does not enforce both proxies")
	}
	if _, _, err := policy.Transport(session.GetWorkspace().GetRepo()); err != nil {
		return nil, fmt.Errorf("compiled Git policy rejects the workspace repository: %w", err)
	}
	read, push := 0, 0
	for _, credential := range session.GetCredentials() {
		if credential.GetHeader() != "authorization" || credential.GetSecretRef().GetKey() != "authorization" {
			continue
		}
		switch credential.GetOrigin() {
		case *policy.ReadProxyOrigin:
			read++
		case *policy.PushProxyOrigin:
			push++
		}
	}
	if read != 1 || push != 1 {
		return nil, errors.New("the Session needs exactly one read and one push Git proxy credential")
	}
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(config)
	if err != nil {
		return nil, err
	}
	environment, composition := session.GetDevelopmentEnvironment(), session.GetRuntimeComposition()
	return &runtimev1.WorkspacePreparation{
		SessionId: session.GetId(), ContextId: session.GetContextId(), PreparedRevision: session.GetPreparedRevision(),
		ActionId: request.GetActionId(), CreateRequestId: request.GetCreateRequestId(), Profile: request.GetSetupProfile(), SetupDigest: request.GetSetupDigest(),
		Workspace:        proto.CloneOf(session.GetWorkspace()),
		DevelopmentImage: environment.GetImage(), Platform: environment.GetPlatform(), PolicyIdentity: environment.GetPolicyIdentity(),
		PayloadImage: composition.GetPayloadImage(), Provider: composition.GetProvider(), Schema: composition.GetSchema(), CliVersion: composition.GetCliVersion(),
		GenerationId: association.GetGenerationId(), Atespace: association.GetAtespace(), ActorName: association.GetActorName(), ActorUid: association.GetActorUid(),
		ConfigDigest: configDigest, McpDigest: mcpDigest,
	}, nil
}

// PrepareWorkspace records a workspace preparation for the Session's current
// runtime and returns its receipt; the runtime settles it asynchronously and
// GetSession reports the result. Only the Session's creator may prepare it, and
// only Sessions that run a selected development environment with a workspace
// through the trusted Git proxies need preparation: session policy already
// admitted their creator and Agent at creation.
func (s *Service) PrepareWorkspace(ctx context.Context, request *apiv1.PrepareSessionWorkspaceRequest) (*apiv1.WorkspacePreparationReceipt, error) {
	if _, shared := auth.ShareContextFrom(ctx); shared {
		return nil, serviceerrors.NewPermissionDenied("A share cannot prepare a workspace", nil)
	}
	if digest, err := apiworkspace.SetupDigest(request.GetSetupProfile()); err != nil || digest != request.GetSetupDigest() {
		return nil, serviceerrors.NewInvalidArgument("Workspace setup digest differs", nil)
	}
	session, err := s.getAuthorized(ctx, request.GetSessionId(), auth.VerbUpdate)
	if err != nil {
		return nil, err
	}
	store, stored := s.store.(preparationStore)
	observer, observes := s.workflow.(associationObserver)
	if !stored || !observes {
		return nil, serviceerrors.NewFailedPrecondition("Workspace preparation is unavailable", nil)
	}
	config, err := store.GetRevisionConfig(ctx, session.GetPreparedRevision())
	if err != nil {
		return nil, serviceerrors.NewInternal("Failed to read the Session revision", err)
	}
	if !database.RequiresWorkspacePreparation(session, config) {
		return nil, serviceerrors.NewPermissionDenied("Session does not need workspace preparation", nil)
	}
	if !database.PreparationMatchesSession(request, session) {
		return nil, serviceerrors.NewFailedPrecondition("Preparation does not match the Session", nil)
	}
	association := observer.RuntimeAssociation(ctx, session)
	if association.GetGenerationId() != request.GetGenerationId() || association.GetActorUid() != request.GetActorUid() {
		s.preparationFailures.warn(ctx, session.GetId(), request.GetGenerationId(), "runtime generation or Actor does not match the request")
		return nil, serviceerrors.NewFailedPrecondition(staleRuntimeMessage, nil)
	}
	if !association.GetCurrentActive() {
		// The runtime polls for its assignment, so it must be awake. Wake it and
		// let the caller retry against the woken runtime.
		if waker, ok := s.workflow.(runtimeWaker); ok {
			if err := waker.WakeRuntime(ctx, session); err != nil {
				logging.FromContext(ctx).WarnContext(ctx, "waking the runtime for workspace preparation failed", "session_id", session.GetId(), "error", err)
			}
		}
		return nil, serviceerrors.NewFailedPrecondition(staleRuntimeMessage, nil)
	}
	assignment, err := preparationAssignment(request, session, association, config)
	if err != nil {
		s.preparationFailures.warn(ctx, session.GetId(), request.GetGenerationId(), err.Error())
		return nil, serviceerrors.NewFailedPrecondition(staleRuntimeMessage, nil)
	}
	receipt, err := store.BeginWorkspacePreparation(ctx, request, assignment, session)
	switch {
	case errors.Is(err, database.ErrIdempotencyConflict):
		return nil, serviceerrors.NewAlreadyExists("Preparation action was used with other inputs", nil)
	case errors.Is(err, database.ErrFailedPrecondition), errors.Is(err, database.ErrConflict):
		return nil, serviceerrors.NewFailedPrecondition(staleRuntimeMessage, nil)
	case errors.Is(err, database.ErrNotFound):
		return nil, serviceerrors.NewNotFound("Session not found", err)
	case err != nil:
		return nil, serviceerrors.NewInternal("Failed to record workspace preparation", err)
	}
	return receipt, nil
}

// preparationFailureLog logs each distinct refusal once per Session and
// generation: callers retry every few seconds, and the caller only ever sees
// the generic refusal.
type preparationFailureLog struct {
	mu   sync.Mutex
	seen map[string]string
}

func (l *preparationFailureLog) warn(ctx context.Context, sessionID, generationID, reason string) {
	key := sessionID + "\x00" + generationID
	l.mu.Lock()
	last, logged := l.seen[key]
	if l.seen == nil {
		l.seen = map[string]string{}
	}
	l.seen[key] = reason
	l.mu.Unlock()
	if logged && last == reason {
		return
	}
	logging.FromContext(ctx).WarnContext(ctx, "workspace preparation refused", "session_id", sessionID, "generation_id", generationID, "reason", reason)
}
