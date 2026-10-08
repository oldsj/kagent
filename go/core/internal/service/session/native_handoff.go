package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type nativePreparationStore interface {
	BeginWorkspacePreparation(context.Context, *api.PrepareSessionWorkspaceRequest, *api.NativeWorkspacePreparation, *api.Session) (*api.WorkspacePreparationReceipt, error)
	GetWorkspacePreparationReceipt(context.Context, string) (*api.WorkspacePreparationReceipt, error)
}

type preparationRuntimeReader interface {
	InspectPreparation(context.Context, *api.Session) (*api.NativeWorkspacePreparation, error)
}

// PrepareWorkspace is a Mainloop-only fixed action, independently of the normal
// Session authorizer. It never sends A2A input or changes the original Create.
func (s *Service) PrepareWorkspace(ctx context.Context, input *api.PrepareSessionWorkspaceRequest) (*api.WorkspacePreparationReceipt, error) {
	principal, ok := auth.AuthSessionFrom(ctx)
	if !ok || principal.Principal().Service != auth.MainloopService || principal.Principal().User.ID != auth.MainloopService || principal.Principal().Agent.ID != "" {
		return nil, serviceerrors.NewPermissionDenied("Workspace preparation requires the Mainloop service principal", nil)
	}
	if _, shared := auth.ShareContextFrom(ctx); shared {
		return nil, serviceerrors.NewPermissionDenied("A share cannot prepare a workspace", nil)
	}
	if input == nil || protovalidate.Validate(input) != nil || proto.Size(input) > 8192 || hasUnknownPreparation(input) {
		return nil, serviceerrors.NewInvalidArgument("Workspace preparation input is invalid", nil)
	}
	digest, err := workspace.SetupDigest(input.GetSetupProfile())
	if err != nil || digest != input.GetSetupDigest() {
		return nil, serviceerrors.NewInvalidArgument("Workspace setup digest differs", nil)
	}
	if _, ok := s.authorizer.(SessionPolicy); !ok {
		return nil, serviceerrors.NewPermissionDenied("Workspace preparation requires an explicit Session policy", nil)
	}
	session, err := s.getAuthorized(ctx, input.GetSessionId(), auth.VerbUpdate)
	if err != nil {
		return nil, err
	}
	if !database.RequiresWorkspacePreparation(session) || session.GetCreator() != auth.MainloopService {
		return nil, serviceerrors.NewPermissionDenied("Session does not own a code preparation profile", nil)
	}
	reader, ok := s.workflow.(preparationRuntimeReader)
	store, stored := s.store.(nativePreparationStore)
	if !ok || !stored {
		return nil, serviceerrors.NewFailedPrecondition("Workspace preparation is unavailable", nil)
	}
	assignment, err := reader.InspectPreparation(ctx, session)
	if err != nil || assignment == nil || assignment.GetGenerationId() != input.GetGenerationId() || assignment.GetActorUid() != input.GetActorUid() {
		return nil, serviceerrors.NewFailedPrecondition("Current prepared runtime identity is unavailable", nil)
	}
	receipt, err := store.BeginWorkspacePreparation(ctx, input, assignment, session)
	switch {
	case errors.Is(err, database.ErrIdempotencyConflict):
		return nil, serviceerrors.NewAlreadyExists("Preparation action was used with other inputs", nil)
	case errors.Is(err, database.ErrFailedPrecondition), errors.Is(err, database.ErrConflict):
		return nil, serviceerrors.NewFailedPrecondition("Session cannot admit this preparation", nil)
	case err != nil:
		return nil, serviceerrors.NewInternal("Failed to assign workspace preparation", nil)
	}
	return receipt, nil
}

func hasUnknownPreparation(input proto.Message) bool {
	unknown := len(input.ProtoReflect().GetUnknown()) != 0
	// The wire is small and has no repeated/map fields. Inspect nested messages too.
	input.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() != nil && hasUnknownPreparation(value.Message().Interface()) {
			unknown = true
		}
		return !unknown
	})
	return unknown
}

// CheckPreparationRuntime compares trusted original preparation, the exact fresh
// Actor and its immutable template. Runtime-reported D/R never supplies defaults.
func CheckPreparationRuntime(session *api.Session, revision *database.RuntimeRevision, actor *ateapipb.Actor, template *ateapipb.ActorTemplate) (*api.NativeWorkspacePreparation, error) {
	if !database.RequiresWorkspacePreparation(session) || revision == nil || !validActorIdentity(actor, revision, actor.GetMetadata().GetName()) || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING || template.GetMetadata().GetUid() != revision.ActorTemplateUID || template.GetMetadata().GetAtespace() != revision.ActorTemplateAtespace || template.GetMetadata().GetName() != revision.ActorTemplateName || len(template.GetContainers()) != 1 {
		return nil, fmt.Errorf("prepared Actor identity differs")
	}
	var original database.EnvironmentRevisionSnapshot
	if err := json.Unmarshal(revision.SourceSnapshot, &original); err != nil || original.BaseRevision == "" || !proto.Equal(original.Environment, session.GetDevelopmentEnvironment()) || !proto.Equal(original.Composition, session.GetRuntimeComposition()) {
		return nil, fmt.Errorf("original prepared D/R differs")
	}
	container := template.Containers[0]
	environment, composition := session.GetDevelopmentEnvironment(), session.GetRuntimeComposition()
	if container.GetImage() != environment.GetImage() || composition.GetSchema() != payload.Schema {
		return nil, fmt.Errorf("prepared runtime image differs")
	}
	var raw []byte
	platform := ""
	for _, variable := range container.GetEnv() {
		if variable.GetName() == "KAGENT_CONFIG_JSON" {
			if raw != nil {
				return nil, fmt.Errorf("duplicate compiled config")
			}
			raw = []byte(variable.GetValue())
		}
		if variable.GetName() == "MAINLOOP_RUNTIME_PLATFORM" {
			platform = variable.GetValue()
		}
	}
	if platform != environment.GetPlatform() {
		return nil, fmt.Errorf("prepared runtime platform differs")
	}
	payloadMatches := 0
	for _, volume := range template.GetVolumes() {
		if volume.GetImage().GetReference() == composition.GetPayloadImage() {
			payloadMatches++
		}
	}
	if payloadMatches != 1 {
		return nil, fmt.Errorf("prepared payload differs")
	}
	var policy *apiworkspace.Git
	var cliVersion string
	switch composition.GetProvider() {
	case "claude":
		parsed, err := claudeconfig.Parse(raw)
		if err != nil {
			return nil, err
		}
		policy, cliVersion = parsed.Git, parsed.ExpectedClaudeVersion
	case "codex":
		parsed, err := codexconfig.Parse(raw)
		if err != nil {
			return nil, err
		}
		policy, cliVersion = parsed.Git, parsed.ExpectedCodexVersion
	default:
		return nil, fmt.Errorf("unsupported native provider")
	}
	if policy == nil || policy.ReadProxyOrigin == nil || policy.PushProxyOrigin == nil || policy.Validate() != nil || cliVersion != composition.GetCliVersion() {
		return nil, fmt.Errorf("original enforcing Git/native configuration differs")
	}
	if _, _, err := policy.Transport(session.GetWorkspace().GetRepo()); err != nil {
		return nil, err
	}
	read, push := 0, 0
	for _, credential := range session.GetCredentials() {
		if credential.GetHeader() == "authorization" && credential.GetSecretRef().GetKey() == "authorization" {
			if credential.GetOrigin() == apiworkspace.ReadProxyOrigin {
				read++
			}
			if credential.GetOrigin() == apiworkspace.PushProxyOrigin {
				push++
			}
		}
	}
	if read != 1 || push != 1 {
		return nil, fmt.Errorf("original Git references differ")
	}
	configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
	if err != nil {
		return nil, err
	}
	return &api.NativeWorkspacePreparation{SessionId: session.Id, ContextId: session.ContextId, PreparedRevision: session.PreparedRevision,
		Workspace: proto.CloneOf(session.Workspace), DevelopmentImage: environment.Image, Platform: environment.Platform, PolicyIdentity: environment.PolicyIdentity,
		PayloadImage: composition.PayloadImage, Provider: composition.Provider, Schema: composition.Schema, CliVersion: composition.CliVersion,
		Atespace: actor.GetMetadata().GetAtespace(), ActorName: actor.GetMetadata().GetName(), ActorUid: actor.GetMetadata().GetUid(), ConfigDigest: configDigest, McpDigest: mcpDigest}, nil
}
