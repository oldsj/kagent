package session

import (
	"context"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/service/serviceerrors"
)

const (
	preparationSessionID = "0193c6a4-7a8e-7d4f-9b2c-1e5f6a7b8c9d"
	readProxy            = "http://mainloop-git-read.mainloop.svc.cluster.local"
	pushProxy            = "http://mainloop-git-push.mainloop.svc.cluster.local"
	preparationConfig    = `{"git":{"origins":["github.com"],"readProxyOrigin":"` + readProxy + `","pushProxyOrigin":"` + pushProxy + `"},` +
		`"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}}`
)

func TestPreparationAssignment(t *testing.T) {
	association := &apiv1.RuntimeAssociation{GenerationId: GenerationID(preparationSessionID, "actor-uid"), Atespace: "team-a", ActorName: "session-x", ActorUid: "actor-uid", Phase: "active", CurrentActive: true}

	t.Run("Given a Session with both proxy credentials, when its assignment is derived and echoed in a completion report, assert that the report passes request validation", func(t *testing.T) {
		assignment, err := preparationAssignment(preparationRequest(), preparationSession(), association, []byte(preparationConfig))
		if err != nil {
			t.Fatalf("preparationAssignment() error = %v", err)
		}
		// The store adds these before issuing the assignment.
		assignment.RequestDigest, assignment.ExecutionId, assignment.ChallengeId = strings.Repeat("3", 64), "0193c6a4-1111-7000-8000-000000000001", "0193c6a4-2222-7000-8000-000000000002"
		report := &runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest{
			SessionId: preparationSessionID, Assignment: assignment, Head: strings.Repeat("a", 40), Branch: "main",
			TransportDigest: strings.Repeat("9", 64), NativeHook: "append_system_prompt", ObservedAt: timestamppb.Now(), Confirmed: true,
		}

		if err := protovalidate.Validate(report); err != nil {
			t.Fatalf("completion report fails validation: %v", err)
		}
	})

	t.Run("Given a Session with both proxy credentials, when its assignment is derived, assert that it carries the request's action and setup and the observed Actor and config digests", func(t *testing.T) {
		assignment, err := preparationAssignment(preparationRequest(), preparationSession(), association, []byte(preparationConfig))

		if err != nil {
			t.Fatalf("preparationAssignment() error = %v", err)
		}
		config, mcp, _ := apiworkspace.ConfigDigests([]byte(preparationConfig))
		if assignment.GetActorUid() != "actor-uid" || assignment.GetGenerationId() != association.GetGenerationId() || assignment.GetConfigDigest() != config || assignment.GetMcpDigest() != mcp ||
			assignment.GetProvider() != "claude" || assignment.GetDevelopmentImage() != preparationSession().GetDevelopmentEnvironment().GetImage() ||
			assignment.GetActionId() != "action-1" || assignment.GetCreateRequestId() != "create-request" || assignment.GetProfile() != "agent" || assignment.GetSetupDigest() != preparationRequest().GetSetupDigest() {
			t.Fatalf("assignment = %v", assignment)
		}
	})

	refusals := []struct {
		name    string
		session func() *apiv1.Session
		config  string
	}{
		{"no push proxy credential", func() *apiv1.Session {
			s := preparationSession()
			s.Credentials = s.Credentials[:1]
			return s
		}, preparationConfig},
		{"two read proxy credentials", func() *apiv1.Session {
			s := preparationSession()
			s.Credentials = append(s.Credentials, proto.CloneOf(s.Credentials[0]))
			return s
		}, preparationConfig},
		{"a credential with another secret key", func() *apiv1.Session {
			s := preparationSession()
			s.Credentials[1].SecretRef.Key = "token"
			return s
		}, preparationConfig},
		{"a revision that reaches Git directly", preparationSession, `{"git":{"origins":["github.com"]},"mcp_servers":{}}`},
		{"a revision without the instance MCP server", preparationSession, `{"git":{"origins":["github.com"],"readProxyOrigin":"` + readProxy + `","pushProxyOrigin":"` + pushProxy + `"},"mcp_servers":{}}`},
	}
	for _, refusal := range refusals {
		t.Run("Given "+refusal.name+", when the assignment is derived, assert that it is refused", func(t *testing.T) {
			if assignment, err := preparationAssignment(preparationRequest(), refusal.session(), association, []byte(refusal.config)); err == nil {
				t.Fatalf("preparationAssignment() = %v, want a refusal", assignment)
			}
		})
	}
}

func TestPrepareWorkspace(t *testing.T) {
	t.Run("Given a running runtime that matches the request, when the workspace is prepared, assert that the store records the derived assignment", func(t *testing.T) {
		f := newPrepareFixture(true)

		receipt, err := f.service.PrepareWorkspace(serviceTestContext("alice"), f.request)

		if err != nil || receipt.GetClassification() != "pending" {
			t.Fatalf("PrepareWorkspace() = %v, %v; want a pending receipt", receipt, err)
		}
		if f.store.begun.GetActorUid() != "actor-uid" || f.store.begun.GetConfigDigest() == "" {
			t.Fatalf("recorded assignment = %v", f.store.begun)
		}
	})

	t.Run("Given a sleeping runtime, when the workspace is prepared, assert that the runtime is woken and the caller is told to retry", func(t *testing.T) {
		f := newPrepareFixture(false)

		_, err := f.service.PrepareWorkspace(serviceTestContext("alice"), f.request)

		if !serviceerrors.IsCode(err, serviceerrors.CodeFailedPrecondition) || serviceerrors.MessageOf(err) != staleRuntimeMessage {
			t.Fatalf("PrepareWorkspace() error = %v, want the stale-runtime refusal", err)
		}
		if f.workflow.woken != 1 || f.store.begun != nil {
			t.Fatalf("woken = %d, recorded = %v; want one wake and nothing recorded", f.workflow.woken, f.store.begun)
		}
	})

	t.Run("Given a running runtime, when the request names another generation, assert that it is refused as stale without a wake", func(t *testing.T) {
		f := newPrepareFixture(true)
		f.request.GenerationId = "0193c6a4-0000-7000-8000-000000000000"

		_, err := f.service.PrepareWorkspace(serviceTestContext("alice"), f.request)

		if !serviceerrors.IsCode(err, serviceerrors.CodeFailedPrecondition) || f.workflow.woken != 0 || f.store.begun != nil {
			t.Fatalf("PrepareWorkspace() error = %v, woken = %d; want a refusal without a wake", err, f.workflow.woken)
		}
	})

	t.Run("Given a valid Session, when the setup digest does not match the profile, assert that the request is invalid", func(t *testing.T) {
		f := newPrepareFixture(true)
		f.request.SetupDigest = strings.Repeat("0", 64)

		_, err := f.service.PrepareWorkspace(serviceTestContext("alice"), f.request)

		if !serviceerrors.IsCode(err, serviceerrors.CodeInvalidArgument) || f.store.begun != nil {
			t.Fatalf("PrepareWorkspace() error = %v, want InvalidArgument", err)
		}
	})

	t.Run("Given a prepared Session, when it is read, assert that the receipt is reported", func(t *testing.T) {
		f := newPrepareFixture(true)
		f.store.receipt = &apiv1.WorkspacePreparationReceipt{Classification: "confirmed"}

		session, err := f.service.Get(serviceTestContext("alice"), preparationSessionID)

		if err != nil || session.GetWorkspacePreparation().GetClassification() != "confirmed" {
			t.Fatalf("Get() = %v, %v; want the confirmed receipt", session.GetWorkspacePreparation(), err)
		}
	})
}

func preparationRequest() *apiv1.PrepareSessionWorkspaceRequest {
	session := preparationSession()
	digest, _ := apiworkspace.SetupDigest("agent")
	return &apiv1.PrepareSessionWorkspaceRequest{
		SessionId: preparationSessionID, ActionId: "action-1", CreateRequestId: "create-request",
		GenerationId: GenerationID(preparationSessionID, "actor-uid"), ActorUid: "actor-uid", PreparedRevision: session.GetPreparedRevision(),
		Workspace: session.GetWorkspace(), DevelopmentEnvironment: session.GetDevelopmentEnvironment(), RuntimeComposition: session.GetRuntimeComposition(),
		SetupProfile: "agent", SetupDigest: digest,
	}
}

func preparationSession() *apiv1.Session {
	return &apiv1.Session{
		Id: preparationSessionID, ContextId: "11111111-1111-4111-8111-111111111111", Creator: "alice",
		State: apiv1.RuntimeState_RUNTIME_STATE_READY, PreparedRevision: "variant",
		Workspace:              &apiv1.Workspace{Repo: "https://github.com/acme/app", Ref: strings.Repeat("a", 40), Branch: "main"},
		DevelopmentEnvironment: &apiv1.DevelopmentEnvironment{Image: "registry.example/dev@sha256:" + strings.Repeat("1", 64), Platform: "linux/arm64", PolicyIdentity: "project/v1"},
		RuntimeComposition:     &apiv1.RuntimeComposition{PayloadImage: "registry.example/runtime@sha256:" + strings.Repeat("2", 64), Provider: "claude", Schema: 1, CliVersion: "2.1.285"},
		Credentials: []*apiv1.SessionCredential{
			{Origin: readProxy, Header: "authorization", SecretRef: &apiv1.SecretKeyReference{Name: "mainloop-git-read-x", Key: "authorization"}},
			{Origin: pushProxy, Header: "authorization", SecretRef: &apiv1.SecretKeyReference{Name: "mainloop-git-push-x", Key: "authorization"}},
		},
	}
}

type prepareFixture struct {
	service  *Service
	store    *preparationTestStore
	workflow *preparationTestWorkflow
	request  *apiv1.PrepareSessionWorkspaceRequest
}

func newPrepareFixture(running bool) *prepareFixture {
	store := &preparationTestStore{serviceTestStore: serviceTestStore{getResult: preparationSession()}}
	workflow := &preparationTestWorkflow{association: &apiv1.RuntimeAssociation{
		GenerationId: GenerationID(preparationSessionID, "actor-uid"), Atespace: "team-a", ActorName: "session-x", ActorUid: "actor-uid", Phase: "active", CurrentActive: running,
	}}
	return &prepareFixture{
		service: NewService(store, serviceTestAuthorizer{}, workflow), store: store, workflow: workflow,
		request: preparationRequest(),
	}
}

type preparationTestStore struct {
	serviceTestStore
	begun   *runtimev1.WorkspacePreparation
	receipt *apiv1.WorkspacePreparationReceipt
}

func (s *preparationTestStore) GetRevisionConfig(context.Context, string) ([]byte, error) {
	return []byte(preparationConfig), nil
}

func (s *preparationTestStore) BeginWorkspacePreparation(_ context.Context, request *apiv1.PrepareSessionWorkspaceRequest, assignment *runtimev1.WorkspacePreparation, _ *apiv1.Session) (*apiv1.WorkspacePreparationReceipt, error) {
	s.begun = assignment
	return &apiv1.WorkspacePreparationReceipt{Original: request, Classification: "pending"}, nil
}

func (s *preparationTestStore) GetWorkspacePreparationReceipt(context.Context, string) (*apiv1.WorkspacePreparationReceipt, error) {
	return s.receipt, nil
}

type preparationTestWorkflow struct {
	serviceTestWorkflow
	association *apiv1.RuntimeAssociation
	woken       int
}

func (w *preparationTestWorkflow) RuntimeAssociation(context.Context, *apiv1.Session) *apiv1.RuntimeAssociation {
	return w.association
}

func (w *preparationTestWorkflow) WakeRuntime(context.Context, *apiv1.Session) error {
	w.woken++
	return nil
}
