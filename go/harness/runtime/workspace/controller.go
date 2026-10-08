package workspace

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// TaskStore is the control-plane client that reports a Session's workspace.
type TaskStore interface {
	GetWorkspace(context.Context) (*apiv1alpha1.Workspace, error)
}

type controllerSource struct{ store TaskStore }

// FromTaskStore reads the workspace request through the runtime's authenticated
// control-plane channel.
func FromTaskStore(store TaskStore) Source { return controllerSource{store: store} }

func (s controllerSource) Workspace(ctx context.Context) (*Request, error) {
	wire, err := s.store.GetWorkspace(ctx)
	if err != nil || wire == nil {
		return nil, err
	}
	return &Request{Repo: wire.GetRepo(), Ref: wire.GetRef(), Branch: wire.GetBranch(), Depth: int(wire.GetDepth())}, nil
}

type unavailableSource struct{}

// Unavailable is a Source that always fails. A configuration check, which never
// serves turns, uses it to validate a Git-enabled runtime without a control plane.
func Unavailable() Source { return unavailableSource{} }

func (unavailableSource) Workspace(context.Context) (*Request, error) {
	return nil, errors.New("workspace source is unavailable")
}

type preparationStore interface {
	GetWorkspacePreparation(context.Context) (*apiv1alpha1.TaskStoreServiceGetWorkspaceResponse, error)
	CompleteWorkspacePreparation(context.Context, *apiv1alpha1.TaskStoreServiceCompleteWorkspacePreparationRequest) error
}

func (s controllerSource) Preparation(ctx context.Context) (PreparationState, error) {
	store, ok := s.store.(preparationStore)
	if !ok {
		return PreparationState{}, nil
	}
	wire, err := store.GetWorkspacePreparation(ctx)
	if err != nil {
		return PreparationState{}, err
	}
	state := PreparationState{Required: wire.GetPreparationRequired(), Ready: wire.GetPreparationReady()}
	if wire.GetPreparation() != nil {
		assignment := PreparationFromWire(wire.Preparation)
		state.Assignment = &assignment
	}
	return state, nil
}

func (s controllerSource) CompletePreparation(ctx context.Context, result PreparationResult) error {
	store, ok := s.store.(preparationStore)
	if !ok {
		return errors.New("preparation callback is unavailable")
	}
	return store.CompleteWorkspacePreparation(ctx, &apiv1alpha1.TaskStoreServiceCompleteWorkspacePreparationRequest{
		SessionId: result.Assignment.SessionID, Assignment: PreparationToWire(result.Assignment), Head: result.HEAD, Branch: result.Branch,
		TransportDigest: result.TransportDigest, NativeHook: result.Hook, ObservedAt: timestamppb.New(result.ObservedAt), Confirmed: result.Confirmed,
	})
}

func PreparationFromWire(wire *apiv1alpha1.NativeWorkspacePreparation) Preparation {
	return Preparation{
		SessionID:        wire.GetSessionId(),
		ContextID:        wire.GetContextId(),
		CreateRequestID:  wire.GetCreateRequestId(),
		ActionID:         wire.GetActionId(),
		RequestDigest:    wire.GetRequestDigest(),
		ExecutionID:      wire.GetExecutionId(),
		ChallengeID:      wire.GetChallengeId(),
		Sequence:         wire.GetSequence(),
		GenerationID:     wire.GetGenerationId(),
		Atespace:         wire.GetAtespace(),
		ActorName:        wire.GetActorName(),
		ActorUID:         wire.GetActorUid(),
		PreparedRevision: wire.GetPreparedRevision(),
		DevelopmentImage: wire.GetDevelopmentImage(),
		Platform:         wire.GetPlatform(),
		PolicyIdentity:   wire.GetPolicyIdentity(),
		PayloadImage:     wire.GetPayloadImage(),
		Provider:         wire.GetProvider(),
		Schema:           wire.GetSchema(),
		CLIVersion:       wire.GetCliVersion(),
		Profile:          wire.GetProfile(),
		SetupDigest:      wire.GetSetupDigest(),
		ConfigDigest:     wire.GetConfigDigest(),
		MCPDigest:        wire.GetMcpDigest(),
		Workspace:        Request{Repo: wire.GetWorkspace().GetRepo(), Ref: wire.GetWorkspace().GetRef(), Branch: wire.GetWorkspace().GetBranch(), Depth: int(wire.GetWorkspace().GetDepth())},
	}
}

func PreparationToWire(input Preparation) *apiv1alpha1.NativeWorkspacePreparation {
	return &apiv1alpha1.NativeWorkspacePreparation{
		SessionId:        input.SessionID,
		ContextId:        input.ContextID,
		CreateRequestId:  input.CreateRequestID,
		ActionId:         input.ActionID,
		RequestDigest:    input.RequestDigest,
		ExecutionId:      input.ExecutionID,
		ChallengeId:      input.ChallengeID,
		Sequence:         input.Sequence,
		GenerationId:     input.GenerationID,
		Atespace:         input.Atespace,
		ActorName:        input.ActorName,
		ActorUid:         input.ActorUID,
		PreparedRevision: input.PreparedRevision,
		DevelopmentImage: input.DevelopmentImage,
		Platform:         input.Platform,
		PolicyIdentity:   input.PolicyIdentity,
		PayloadImage:     input.PayloadImage,
		Provider:         input.Provider,
		Schema:           input.Schema,
		CliVersion:       input.CLIVersion,
		Profile:          input.Profile,
		SetupDigest:      input.SetupDigest,
		ConfigDigest:     input.ConfigDigest,
		McpDigest:        input.MCPDigest,
		Workspace:        &apiv1alpha1.Workspace{Repo: input.Workspace.Repo, Ref: input.Workspace.Ref, Branch: input.Workspace.Branch, Depth: int32(input.Workspace.Depth)},
	}
}
