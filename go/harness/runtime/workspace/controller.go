package workspace

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
)

// TaskStore is the control-plane client that reports a Session's workspace.
type TaskStore interface {
	GetWorkspace(context.Context) (*apiv1.Workspace, error)
}

type controllerSource struct{ store TaskStore }

// ConversationOf returns the Source's view of whether the Session starts a
// fresh native conversation, or nil when the Source has none (a configuration
// check, or a control plane that cannot say).
func ConversationOf(source Source) continuation.FreshSource {
	fresh, _ := source.(continuation.FreshSource)
	return fresh
}

type conversationStore interface {
	SessionID() (string, error)
	FreshConversation(context.Context) (bool, error)
}

func (s controllerSource) SessionID() (string, error) {
	store, ok := s.store.(conversationStore)
	if !ok {
		return "", errors.New("session identity is unavailable")
	}
	return store.SessionID()
}

func (s controllerSource) FreshConversation(ctx context.Context) (bool, error) {
	store, ok := s.store.(conversationStore)
	if !ok {
		return false, errors.New("fresh conversation flag is unavailable")
	}
	return store.FreshConversation(ctx)
}

// FromTaskStore reads the workspace request through the runtime's authenticated
// control-plane channel.
// When store can also read and complete preparations, the Source is a
// PreparationSource.
func FromTaskStore(store TaskStore) Source {
	if prepared, ok := store.(PreparationStore); ok {
		return preparationSource{controllerSource{store: store}, prepared}
	}
	return controllerSource{store: store}
}

func (s controllerSource) Workspace(ctx context.Context) (*Request, error) {
	return requestFromWire(s.store.GetWorkspace(ctx))
}

func requestFromWire(wire *apiv1.Workspace, err error) (*Request, error) {
	if err != nil || wire == nil {
		return nil, err
	}
	return &Request{Repo: wire.GetRepo(), Ref: wire.GetRef(), Branch: wire.GetBranch(), Depth: int(wire.GetDepth()), ServiceCommand: slices.Clone(wire.GetServiceCommand())}, nil
}

// PreparationStore is the control-plane client that assigns and completes a
// Session's workspace preparation.
type PreparationStore interface {
	GetWorkspacePreparation(context.Context) (*runtimev1.TaskStoreServiceGetWorkspaceResponse, error)
	CompleteWorkspacePreparation(context.Context, *runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest) error
}

type preparationSource struct {
	controllerSource
	store PreparationStore
}

func (s preparationSource) Preparation(ctx context.Context) (PreparationState, error) {
	response, err := s.store.GetWorkspacePreparation(ctx)
	if err != nil {
		return PreparationState{}, err
	}
	state := PreparationState{Required: response.GetPreparationRequired(), Ready: response.GetPreparationReady()}
	if wire := response.GetPreparation(); wire != nil {
		assignment := PreparationFromWire(wire)
		state.Assignment = &assignment
	}
	return state, nil
}

func (s preparationSource) CompletePreparation(ctx context.Context, result PreparationResult) error {
	return s.store.CompleteWorkspacePreparation(ctx, &runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest{
		SessionId:       result.Assignment.SessionID,
		Assignment:      PreparationToWire(result.Assignment),
		Head:            result.HEAD,
		Branch:          result.Branch,
		TransportDigest: result.TransportDigest,
		NativeHook:      result.Hook,
		ObservedAt:      timestamppb.New(result.ObservedAt),
		Confirmed:       result.Confirmed,
	})
}

// PreparationFromWire maps every field of an assigned preparation.
func PreparationFromWire(wire *runtimev1.WorkspacePreparation) Preparation {
	var request Request
	if workspace, _ := requestFromWire(wire.GetWorkspace(), nil); workspace != nil {
		request = *workspace
	}
	return Preparation{
		SessionID: wire.GetSessionId(), ContextID: wire.GetContextId(), CreateRequestID: wire.GetCreateRequestId(),
		ActionID: wire.GetActionId(), RequestDigest: wire.GetRequestDigest(), ExecutionID: wire.GetExecutionId(),
		ChallengeID: wire.GetChallengeId(), Sequence: wire.GetSequence(), GenerationID: wire.GetGenerationId(),
		Atespace: wire.GetAtespace(), ActorName: wire.GetActorName(), ActorUID: wire.GetActorUid(),
		PreparedRevision: wire.GetPreparedRevision(), Workspace: request,
		DevelopmentImage: wire.GetDevelopmentImage(), Platform: wire.GetPlatform(), PolicyIdentity: wire.GetPolicyIdentity(),
		PayloadImage: wire.GetPayloadImage(), Provider: wire.GetProvider(), Schema: wire.GetSchema(),
		CLIVersion: wire.GetCliVersion(), Profile: wire.GetProfile(), SetupDigest: wire.GetSetupDigest(),
		ConfigDigest: wire.GetConfigDigest(), MCPDigest: wire.GetMcpDigest(),
	}
}

// PreparationToWire is the inverse of PreparationFromWire, so a completion
// echoes the exact assignment it answers.
func PreparationToWire(p Preparation) *runtimev1.WorkspacePreparation {
	return &runtimev1.WorkspacePreparation{
		SessionId: p.SessionID, ContextId: p.ContextID, CreateRequestId: p.CreateRequestID,
		ActionId: p.ActionID, RequestDigest: p.RequestDigest, ExecutionId: p.ExecutionID,
		ChallengeId: p.ChallengeID, Sequence: p.Sequence, GenerationId: p.GenerationID,
		Atespace: p.Atespace, ActorName: p.ActorName, ActorUid: p.ActorUID,
		PreparedRevision: p.PreparedRevision,
		Workspace: &apiv1.Workspace{
			Repo: p.Workspace.Repo, Ref: p.Workspace.Ref, Branch: p.Workspace.Branch, Depth: int32(p.Workspace.Depth), //nolint:gosec // Depth came from an int32 and is bounded by MaxDepth.
			ServiceCommand: slices.Clone(p.Workspace.ServiceCommand),
		},
		DevelopmentImage: p.DevelopmentImage, Platform: p.Platform, PolicyIdentity: p.PolicyIdentity,
		PayloadImage: p.PayloadImage, Provider: p.Provider, Schema: p.Schema,
		CliVersion: p.CLIVersion, Profile: p.Profile, SetupDigest: p.SetupDigest,
		ConfigDigest: p.ConfigDigest, McpDigest: p.MCPDigest,
	}
}

type unavailableSource struct{}

// Unavailable is a Source that always fails. A configuration check, which never
// serves turns, uses it to validate a Git-enabled runtime without a control plane.
func Unavailable() Source { return unavailableSource{} }

func (unavailableSource) Workspace(context.Context) (*Request, error) {
	return nil, errors.New("workspace source is unavailable")
}
