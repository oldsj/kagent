package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type workflowStore interface {
	AllocateRuntimeGeneration(context.Context, database.RuntimeGeneration) (*database.RuntimeGeneration, bool, error)
	GetRuntimeGeneration(context.Context, string) (*database.RuntimeGeneration, error)
	AdvanceRuntimeGeneration(context.Context, uuid.UUID, string, string, string) error
	RevokeRuntimeGeneration(context.Context, string) error
	SessionGenerationEligible(context.Context, string) (bool, error)
	ClaimSessionQuiescence(context.Context) (*database.SessionQuiescence, error)
	FinishSessionQuiescence(context.Context, *database.SessionQuiescence, *database.SessionTaskSnapshot) error
	GetSessionForRuntime(context.Context, string, string) (*apiv1alpha1.Session, error)
	GetSessionCheckpointSnapshot(context.Context, string, string) (*database.SessionTaskSnapshot, string, error)
	GetRuntimeRevision(context.Context, string) (*database.RuntimeRevision, error)
	BeginSessionOperation(context.Context, string, apiv1alpha1.RuntimeOperation) (*database.SessionOperation, error)
	ClaimSessionOperation(context.Context, string, uuid.UUID, uuid.UUID) (bool, error)
	ReleaseRuntimeOperation(context.Context, string, uuid.UUID, uuid.UUID) error
	FinishSessionOperation(context.Context, string, uuid.UUID, uuid.UUID, string, string, string) (*apiv1alpha1.Session, error)
	GetSessionOperation(context.Context, string, uuid.UUID) (*database.SessionOperation, error)
}

type actorClient interface {
	substrate.LifecycleClient
	PauseActor(context.Context, string, string) (*ateapipb.Actor, error)
}

// ActorWorkflow runs the imperative Substrate operations behind Session
// lifecycle RPCs. Only the claiming caller issues lifecycle mutations; others
// observe current completion or receive a pending/superseded-operation error.
type runtimeCredentials interface {
	Namespace() string
	Ensure(context.Context, database.RuntimeGeneration, string) error
	Delete(context.Context, database.RuntimeGeneration) error
}

var _ runtimeCredentials = (*substrate.RuntimeCredentialIssuer)(nil)

type ActorWorkflow struct {
	credentials    runtimeCredentials
	callbackOrigin string
	store          workflowStore
	actors         actorClient
}

func NewActorWorkflow(store workflowStore, actors actorClient, credentials runtimeCredentials, callbackOrigin string) *ActorWorkflow {
	return &ActorWorkflow{store: store, actors: actors, credentials: credentials, callbackOrigin: callbackOrigin}
}

var _ runtimeAssociationReader = (*ActorWorkflow)(nil)

// InspectRuntime observes only; it never reconciles Actors or credentials and
// holds no lifecycle locks across the network read. Durable checks surround the
// Actor lookup, but changes after these reads remain possible. This is not a
// cross-system authorization grant; dispatch must apply its own normal fencing.
func (w *ActorWorkflow) InspectRuntime(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.RuntimeAssociation, error) {
	if w == nil {
		return nil, nil
	}
	if !activeRuntimeSession(session, session) {
		return nil, nil
	}
	generation, err := w.store.GetRuntimeGeneration(ctx, session.GetId())
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime generation: %w", err)
	}
	if !activeRuntimeGeneration(generation, session) {
		return nil, nil
	}
	current, err := w.store.GetSessionForRuntime(ctx, session.GetId(), generation.ActorUID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime Session: %w", err)
	}
	if !activeRuntimeSession(current, session) {
		return nil, nil
	}
	actor, err := w.actors.GetActor(ctx, generation.Atespace, generation.ActorName)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to observe runtime Actor: %w", err)
	}
	metadata := actor.GetMetadata()
	if metadata.GetAtespace() != generation.Atespace || metadata.GetName() != generation.ActorName || metadata.GetUid() != generation.ActorUID || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return nil, nil
	}
	current, err = w.store.GetSessionForRuntime(ctx, session.GetId(), generation.ActorUID)
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to recheck runtime Session: %w", err)
	}
	if !activeRuntimeSession(current, session) {
		return nil, nil
	}
	observed, err := w.store.GetRuntimeGeneration(ctx, session.GetId())
	if errors.Is(err, database.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to recheck runtime generation: %w", err)
	}
	if !activeRuntimeGeneration(observed, session) || observed.ID != generation.ID || observed.Atespace != generation.Atespace || observed.ActorName != generation.ActorName || observed.ActorUID != generation.ActorUID {
		return nil, nil
	}
	return &apiv1alpha1.RuntimeAssociation{
		GenerationId: generation.ID.String(), Atespace: generation.Atespace,
		ActorName: generation.ActorName, ActorUid: generation.ActorUID,
		Phase: generation.Phase, CurrentActive: true,
	}, nil
}

func activeRuntimeGeneration(generation *database.RuntimeGeneration, session *apiv1alpha1.Session) bool {
	return generation != nil && generation.ID != uuid.Nil && generation.SessionID.String() == session.GetId() && generation.Phase == "active" && generation.Atespace != "" && generation.ActorName != "" && generation.ActorUID != ""
}

func activeRuntimeSession(current, expected *apiv1alpha1.Session) bool {
	return current != nil && current.GetId() == expected.GetId() && current.GetCreator() == expected.GetCreator() && current.GetPreparedRevision() == expected.GetPreparedRevision() &&
		current.GetAgent().GetNamespace() == expected.GetAgent().GetNamespace() && current.GetAgent().GetName() == expected.GetAgent().GetName() &&
		current.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_READY && current.GetOperation() == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE
}

// Pause checkpoints the runtime on its current worker without changing the
// Session logical state or persisting A2A task state.
func (w *ActorWorkflow) Pause(ctx context.Context, session *apiv1alpha1.Session) error {
	revision, err := w.store.GetRuntimeRevision(ctx, session.GetPreparedRevision())
	if err != nil {
		return fmt.Errorf("load prepared revision: %w", err)
	}
	generation, err := w.store.GetRuntimeGeneration(ctx, session.GetId())
	if err != nil || generation.Phase != "active" {
		return fmt.Errorf("runtime generation unavailable")
	}
	atespace, name := generation.Atespace, generation.ActorName
	current, err := w.actors.GetActor(ctx, atespace, name)
	if err != nil {
		return err
	}
	if err := w.verifyActor(ctx, session, revision, current); err != nil {
		return err
	}
	actor, err := w.actors.PauseActor(ctx, atespace, name)
	if err != nil {
		return fmt.Errorf("pause Actor %s/%s: %w", atespace, name, err)
	}
	if !validActorIdentity(actor, revision, name) || actor.GetMetadata().GetUid() != current.GetMetadata().GetUid() || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_PAUSED {
		return fmt.Errorf("pause Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
	}
	return nil
}

// Quiesce durably suspends the runtime without changing the Session's
// logical READY state and records its external snapshot URI. Only a checkpoint
// retains a copy after the Actor advances.
func (w *ActorWorkflow) Quiesce(ctx context.Context, session *apiv1alpha1.Session) (*database.SessionTaskSnapshot, error) {
	revision, err := w.store.GetRuntimeRevision(ctx, session.GetPreparedRevision())
	if err != nil {
		return nil, fmt.Errorf("load prepared revision: %w", err)
	}
	generation, err := w.store.GetRuntimeGeneration(ctx, session.GetId())
	if err != nil || generation.Phase != "active" {
		return nil, fmt.Errorf("runtime generation unavailable")
	}
	atespace, name := generation.Atespace, generation.ActorName
	current, err := w.actors.GetActor(ctx, atespace, name)
	if err != nil {
		return nil, err
	}
	if err := w.verifyActor(ctx, session, revision, current); err != nil {
		return nil, err
	}
	actor, err := w.actors.SuspendActor(ctx, atespace, name)
	if err != nil {
		return nil, fmt.Errorf("suspend Actor %s/%s: %w", atespace, name, err)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return nil, fmt.Errorf("suspend Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
	}
	metadata := actor.GetMetadata()
	if !validActorIdentity(actor, revision, name) || metadata.GetUid() != current.GetMetadata().GetUid() {
		return nil, fmt.Errorf("suspend actor %s/%s returned invalid identity", atespace, name)
	}
	snapshot := actor.GetStatus().GetExternalSnapshot()
	if snapshot.GetSnapshotUri() == "" {
		return nil, fmt.Errorf("suspend Actor %s/%s returned no snapshot", atespace, name)
	}
	scope := snapshot.GetContentScope()
	if scope != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA {
		return nil, fmt.Errorf("actor %s/%s returned invalid snapshot content scope %s", atespace, name, scope)
	}
	return &database.SessionTaskSnapshot{
		Atespace: atespace, URI: snapshot.GetSnapshotUri(),
		ContentScope: strings.TrimPrefix(scope.String(), "SNAPSHOT_CONTENT_SCOPE_"),
	}, nil
}

// Create provisions the persisted session once, using its pinned checkpoint for
// forks. Retries return current state; an uncertain prior creation blocks execution.
func (w *ActorWorkflow) Create(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	return w.run(ctx, session.GetId(), apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
}

// Suspend returns after the Actor and session are suspended. A retry observes
// the same operation rather than issuing a second mutation.
func (w *ActorWorkflow) Suspend(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	return w.run(ctx, session.GetId(), apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
}

// Resume returns after the Actor is running and the session is ready. Missing
// Actors are errors; Resume never creates replacement compute.
func (w *ActorWorkflow) Resume(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	return w.run(ctx, session.GetId(), apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME)
}

// Delete closes admission before stopping and deleting compute. It can supersede
// unissued creation, but never deletes a session while a prior call is uncertain.
func (w *ActorWorkflow) Delete(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.Session, error) {
	return w.run(ctx, session.GetId(), apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
}

func (w *ActorWorkflow) run(ctx context.Context, sessionID string, requestedKind apiv1alpha1.RuntimeOperation) (*apiv1alpha1.Session, error) {
	ctx, cancelAttempt := context.WithTimeout(ctx, database.RuntimeOperationTimeout)
	defer cancelAttempt()
	operation, err := w.store.BeginSessionOperation(ctx, sessionID, requestedKind)
	if err != nil {
		return nil, err
	}
	return w.execute(ctx, operation)
}

// execute keeps lifecycle preparation separate from the durable issue boundary.
// Multiple callers may prepare using read-only calls; exactly one can authorize
// runtime mutations for a bounded attempt. Errors retain the operation and its
// resource pins so a client retry can continue it. Session admission still
// serializes lifecycle against runtime writes, idle work, and checkpoints.
func (w *ActorWorkflow) execute(ctx context.Context, operation *database.SessionOperation) (_ *apiv1alpha1.Session, err error) {
	sessionID := operation.Instance.Id
	if operation.Instance.Operation == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return operationOutcome(operation)
	}
	kind := operation.Instance.Operation
	session := operation.Instance
	revision, err := w.store.GetRuntimeRevision(ctx, session.GetPreparedRevision())
	if err != nil {
		return w.failPreparation(ctx, operation, fmt.Errorf("load prepared revision: %w", err))
	}
	var snapshot *database.SessionTaskSnapshot
	var tagName string
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE && operation.SourceCheckpointID != nil {
		snapshot, _, err = w.store.GetSessionCheckpointSnapshot(ctx, operation.SourceCheckpointID.String(), session.Creator)
		if err != nil {
			return w.failPreparation(ctx, operation, fmt.Errorf("load pinned checkpoint: %w", err))
		}
		if snapshot == nil || snapshot.URI == "" || snapshot.Atespace == "" || snapshot.ContentScope != "DATA" {
			return w.failPreparation(ctx, operation, fmt.Errorf("fork requires a retained DATA checkpoint"))
		}
		tagName = "checkpoint-" + operation.SourceCheckpointID.String()
	}

	var generation *database.RuntimeGeneration
	var originalToken string
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
		candidate, token, candidateErr := substrate.NewRuntimeGeneration(sessionID, revision.ActorTemplateAtespace, w.credentials.Namespace())
		if candidateErr != nil {
			return nil, candidateErr
		}
		credentials, validationErr := egress.SessionCredentials(session.GetAgent().GetNamespace(), session.GetCredentials(), revision.EgressDestinations, revision.Credentials)
		if validationErr != nil {
			return w.failPreparation(ctx, operation, validationErr)
		}
		if _, validationErr = substrate.RuntimeEgressPolicy(candidate, w.callbackOrigin, revision.EgressDestinations, credentials); validationErr != nil {
			return w.failPreparation(ctx, operation, validationErr)
		}
		var allocated bool
		generation, allocated, err = w.store.AllocateRuntimeGeneration(ctx, candidate)
		if allocated {
			originalToken = token
		}
	} else {
		generation, err = w.store.GetRuntimeGeneration(ctx, sessionID)
	}
	if errors.Is(err, database.ErrNotFound) && kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
		eligible, eligibilityErr := w.store.SessionGenerationEligible(ctx, sessionID)
		if eligibilityErr != nil || !eligible {
			return nil, fmt.Errorf("unledgered Session deletion requires fresh issuance provenance")
		}
		// No ledger means this fresh Session has never issued runtime effects.
		executorID := uuid.New()
		claimed, claimErr := w.store.ClaimSessionOperation(ctx, sessionID, operation.ID, executorID)
		if claimErr != nil {
			return nil, claimErr
		}
		if !claimed {
			return nil, database.ErrConflict
		}
		result, finishErr := w.store.FinishSessionOperation(ctx, sessionID, operation.ID, executorID, "", "", "")
		if finishErr != nil {
			return nil, errors.Join(finishErr, w.store.ReleaseRuntimeOperation(context.WithoutCancel(ctx), sessionID, operation.ID, executorID))
		}
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runtime generation unavailable: %w", err)
	}
	if generation.Phase == "revoked" && kind != apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
		return nil, database.ErrFailedPrecondition
	}
	if generation.Phase == "actor-issued" {
		return nil, fmt.Errorf("actor issuance outcome unknown; original generation held")
	}
	if kind != apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE && kind != apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE && generation.Phase != "active" {
		return nil, database.ErrFailedPrecondition
	}
	binding := substrate.ActorBinding{ExpectedUID: generation.ActorUID, Atespace: generation.Atespace, Name: generation.ActorName,
		TemplateAtespace: revision.ActorTemplateAtespace, TemplateName: revision.ActorTemplateName}
	var creation *substrate.ActorCreation
	var runtimePolicy *ateapipb.EgressPolicy
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE || kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME {
		credentials, err := egress.SessionCredentials(session.GetAgent().GetNamespace(), session.GetCredentials(), revision.EgressDestinations, revision.Credentials)
		if err != nil {
			return w.failPreparation(ctx, operation, err)
		}
		policy, err := substrate.RuntimeEgressPolicy(*generation, w.callbackOrigin, revision.EgressDestinations, credentials)
		if err != nil {
			return w.failPreparation(ctx, operation, err)
		}
		runtimePolicy = policy
		if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
			creation = &substrate.ActorCreation{EgressPolicy: policy}
			if snapshot != nil {
				creation.Snapshot = &substrate.ActorSnapshot{Tag: &ateapipb.ObjectRef{Atespace: snapshot.Atespace, Name: tagName}, URI: snapshot.URI, ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}
			}
		}
	}
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
		if err := w.credentials.Ensure(ctx, *generation, originalToken); err != nil {
			return nil, err
		}
		if generation.Phase == "allocated" {
			if err := w.store.AdvanceRuntimeGeneration(ctx, generation.ID, "allocated", "secret-issued", ""); err != nil {
				return nil, err
			}
			generation.Phase = "secret-issued"
		}
	}

	prepare := substrate.PrepareActorTransition
	if operation.ExecutorID != uuid.Nil && generation.ActorUID != "" {
		prepare = substrate.PrepareActorRetry
	}
	transition, err := prepare(ctx, w.actors, binding, kind, creation)
	if err != nil {
		return w.failPreparation(ctx, operation, err)
	}

	if uid := transition.ActorUID(); uid != "" && kind != apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
		if _, err := w.store.GetSessionForRuntime(ctx, session.Id, uid); err != nil {
			return w.failPreparation(ctx, operation, fmt.Errorf("verify runtime actor UID: %w", err))
		}
	}

	executorID := uuid.New()
	claimed, err := w.store.ClaimSessionOperation(ctx, sessionID, operation.ID, executorID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		// A superseded generation returns a conflict without runtime work.
		current, err := w.store.GetSessionOperation(ctx, sessionID, operation.ID)
		if err != nil {
			return nil, err
		}
		return operationOutcome(current)
	}
	defer func() {
		if err == nil {
			return // Completion already cleared the claim.
		}
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, w.store.ReleaseRuntimeOperation(finishCtx, sessionID, operation.ID, executorID))
	}()
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
		if err := w.store.RevokeRuntimeGeneration(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
		if generation.Phase == "secret-issued" {
			// Only a newly committed issuance transition authorizes creation.
			// A stale phase or uncertain write reply must hold without issuing.
			if err := w.store.AdvanceRuntimeGeneration(ctx, generation.ID, "secret-issued", "actor-issued", ""); err != nil {
				return nil, err
			}
			uid, err := substrate.IssueActorCreation(ctx, w.actors, transition)
			if err != nil {
				return nil, fmt.Errorf("actor issuance uncertain; generation held: %w", err)
			}
			if err := w.store.AdvanceRuntimeGeneration(ctx, generation.ID, "actor-issued", "bound", uid); err != nil {
				return nil, err
			}
			generation.ActorUID, generation.Phase = uid, "bound"
		}
		if generation.Phase != "bound" && generation.Phase != "active" {
			return nil, database.ErrFailedPrecondition
		}
		if err := w.actors.EnsureActorEgressPolicy(ctx, binding.Atespace, binding.Name, creation.EgressPolicy); err != nil {
			return nil, err
		}
		if generation.Phase == "bound" {
			if err := w.store.AdvanceRuntimeGeneration(ctx, generation.ID, "bound", "active", generation.ActorUID); err != nil {
				return nil, err
			}
		}
	}
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_RESUME {
		if err := w.credentials.Ensure(ctx, *generation, ""); err != nil {
			return nil, err
		}
		if err := w.actors.EnsureActorEgressPolicy(ctx, generation.Atespace, generation.ActorName, runtimePolicy); err != nil {
			return nil, err
		}
		current, err := w.store.GetRuntimeGeneration(ctx, sessionID)
		if err != nil || current.ID != generation.ID || current.Phase != "active" || current.ActorUID != generation.ActorUID {
			return nil, fmt.Errorf("runtime capability changed before resume")
		}
	}
	if err := substrate.ApplyActorTransition(ctx, w.actors, transition); err != nil {
		return nil, fmt.Errorf("lifecycle operation %s remains pending: %w", operation.ID, err)
	}
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
		if err := w.credentials.Delete(ctx, *generation); err != nil {
			return nil, err
		}
	}
	var authority string
	if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE {
		authority = substrate.ActorHost(binding.Atespace, binding.Name, "")
	}

	// A disconnected client must not discard an already known runtime outcome.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.store.FinishSessionOperation(finishCtx, sessionID, operation.ID, executorID, authority, transition.ActorUID(), "")
}

// failPreparation releases only unissued work. If another caller won, observe
// the same generation instead. Completion returns current state; supersession
// returns a conflict. A local preparation error cannot clear a newer operation.
func (w *ActorWorkflow) failPreparation(ctx context.Context, admitted *database.SessionOperation, cause error) (*apiv1alpha1.Session, error) {
	if admitted.ExecutorID != uuid.Nil {
		return nil, cause // An earlier attempt may have issued work; retain its intent.
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := w.store.FinishSessionOperation(finishCtx, admitted.Instance.Id, admitted.ID, uuid.Nil, "", "", "lifecycle preparation failed")
	if errors.Is(err, database.ErrConflict) {
		operation, readErr := w.store.GetSessionOperation(finishCtx, admitted.Instance.Id, admitted.ID)
		if readErr != nil {
			return nil, errors.Join(cause, err, readErr)
		}
		return operationOutcome(operation)
	}
	return nil, errors.Join(cause, err)
}

func operationOutcome(operation *database.SessionOperation) (*apiv1alpha1.Session, error) {
	if operation.Instance.Operation == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return operation.Instance, nil
	}
	return nil, fmt.Errorf("lifecycle operation %s is pending; runtime effects may be unresolved: %w", operation.ID, database.ErrConflict)
}

// Verify the recorded actor UID before issuing lifecycle work. Names and
// templates alone also match an externally replaced actor, which we must not
// adopt or checkpoint as this session's runtime.
func (w *ActorWorkflow) verifyActor(ctx context.Context, session *apiv1alpha1.Session, revision *database.RuntimeRevision, actor *ateapipb.Actor) error {
	generation, err := w.store.GetRuntimeGeneration(ctx, session.Id)
	if err != nil || generation.Phase != "active" || actor.GetMetadata().GetUid() != generation.ActorUID || !validActorIdentity(actor, revision, generation.ActorName) {
		return fmt.Errorf("runtime actor identity or template changed")
	}
	if _, err := w.store.GetSessionForRuntime(ctx, session.Id, actor.GetMetadata().GetUid()); err != nil {
		return fmt.Errorf("verify runtime actor UID: %w", err)
	}
	return nil
}

func validActorIdentity(actor *ateapipb.Actor, revision *database.RuntimeRevision, name string) bool {
	metadata, ref := actor.GetMetadata(), actor.GetActorTemplate()
	return metadata.GetName() == name && metadata.GetAtespace() == revision.ActorTemplateAtespace && metadata.GetUid() != "" &&
		ref.GetAtespace() == revision.ActorTemplateAtespace && ref.GetName() == revision.ActorTemplateName
}

// InspectPreparation adds fixed template/config verification to the existing
// current-generation observation, without holding a lock across Actor reads.
func (w *ActorWorkflow) InspectPreparation(ctx context.Context, session *apiv1alpha1.Session) (*apiv1alpha1.NativeWorkspacePreparation, error) {
	association, err := w.InspectRuntime(ctx, session)
	if err != nil || association == nil {
		return nil, err
	}
	reader, ok := w.actors.(interface {
		GetActorTemplate(context.Context, string, string) (*ateapipb.ActorTemplate, error)
	})
	if !ok {
		return nil, fmt.Errorf("prepared template observation unavailable")
	}
	revision, err := w.store.GetRuntimeRevision(ctx, session.GetPreparedRevision())
	if err != nil {
		return nil, err
	}
	actor, err := w.actors.GetActor(ctx, association.Atespace, association.ActorName)
	if err != nil || actor.GetMetadata().GetUid() != association.ActorUid {
		return nil, fmt.Errorf("runtime identity changed")
	}
	template, err := reader.GetActorTemplate(ctx, revision.ActorTemplateAtespace, revision.ActorTemplateName)
	if err != nil {
		return nil, err
	}
	result, err := CheckPreparationRuntime(session, revision, actor, template)
	if err != nil {
		return nil, err
	}
	current, err := w.InspectRuntime(ctx, session)
	if err != nil || current == nil || current.GenerationId != association.GenerationId || current.ActorUid != association.ActorUid {
		return nil, fmt.Errorf("runtime changed around observation")
	}
	result.GenerationId = association.GenerationId
	return result, nil
}
