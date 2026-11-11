package database

import (
	"context"
	"strings"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kagent-dev/kagent/go/api/bootstrap"
	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
)

const preparationConfig = `{"git":{"origins":["github.com"],"readProxyOrigin":"http://mainloop-git-read.mainloop.svc.cluster.local",` +
	`"pushProxyOrigin":"http://mainloop-git-push.mainloop.svc.cluster.local"},"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}}`

func TestWorkspacePreparation(t *testing.T) {
	t.Run("Given a prepared Session, when the runtime reads, completes, and the caller reads again, assert that the receipt is confirmed with the runtime's facts", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)

		pending, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		require.Equal(t, PreparationPending, pending.GetClassification())
		state, err := f.client.IssueWorkspacePreparation(f.ctx, f.session, "actor-uid")
		require.NoError(t, err)
		require.True(t, state.Required)
		require.False(t, state.Ready)
		require.NotNil(t, state.Assignment)
		require.NoError(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", f.confirmed(state.Assignment)))
		receipt, err := f.client.GetWorkspacePreparationReceipt(f.ctx, f.session.GetId())

		require.NoError(t, err)
		require.Equal(t, PreparationConfirmed, receipt.GetClassification())
		require.Equal(t, strings.Repeat("a", 40), receipt.GetHead())
		require.Equal(t, "main", receipt.GetBranch())
		require.Equal(t, "append_system_prompt", receipt.GetNativeHook())
		require.NotNil(t, receipt.GetEffectObservedAt())
		require.NotNil(t, receipt.GetObservedAt())
		require.False(t, receipt.GetHistorical())
		require.True(t, proto.Equal(f.request, receipt.GetOriginal()))
		ready, err := f.client.IssueWorkspacePreparation(f.ctx, f.session, "actor-uid")
		require.NoError(t, err)
		require.True(t, ready.Ready)
		require.Nil(t, ready.Assignment)
	})

	t.Run("Given a recorded preparation, when the same action is retried with the same and then other bytes, assert that the first returns it and the second is an idempotency conflict", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)
		first, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)

		again, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		require.Equal(t, first.GetExecutionId(), again.GetExecutionId())
		other := proto.CloneOf(f.request)
		other.SetupProfile = "child"
		_, err = f.client.BeginWorkspacePreparation(f.ctx, other, f.assignment, f.session)

		require.ErrorIs(t, err, ErrIdempotencyConflict)
	})

	scenarios := []struct {
		name   string
		mutate func(*apiv1.PrepareSessionWorkspaceRequest, *runtimev1.WorkspacePreparation)
	}{
		{"a stale Actor UID", func(r *apiv1.PrepareSessionWorkspaceRequest, a *runtimev1.WorkspacePreparation) {
			r.ActorUid, a.ActorUid = "replaced-uid", "replaced-uid"
		}},
		{"another creation request", func(r *apiv1.PrepareSessionWorkspaceRequest, _ *runtimev1.WorkspacePreparation) {
			r.CreateRequestId = "other-request"
		}},
		{"another workspace", func(r *apiv1.PrepareSessionWorkspaceRequest, _ *runtimev1.WorkspacePreparation) {
			r.Workspace.Branch = "other"
		}},
	}
	for _, scenario := range scenarios {
		t.Run("Given a ready Session, when a preparation names "+scenario.name+", assert that it fails its precondition", func(t *testing.T) {
			f := newPreparationFixture(t, preparationConfig)
			request, assignment := proto.CloneOf(f.request), proto.CloneOf(f.assignment)
			scenario.mutate(request, assignment)

			_, err := f.client.BeginWorkspacePreparation(f.ctx, request, assignment, f.session)

			require.ErrorIs(t, err, ErrFailedPrecondition)
		})
	}

	t.Run("Given a Session whose revision reaches Git directly, when a preparation is requested, assert that it fails its precondition and turns are admitted", func(t *testing.T) {
		f := newPreparationFixture(t, `{"git":{"origins":["github.com"]}}`)

		_, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)

		require.ErrorIs(t, err, ErrFailedPrecondition)
		require.NoError(t, f.createTask("direct"))
	})

	t.Run("Given a Session that requires preparation, when a turn starts before and after confirmation, assert that only the confirmed turn is admitted", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)

		require.ErrorIs(t, f.createTask("unprepared"), ErrFailedPrecondition)
		_, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		state, err := f.client.IssueWorkspacePreparation(f.ctx, f.session, "actor-uid")
		require.NoError(t, err)
		require.ErrorIs(t, f.createTask("pending"), ErrFailedPrecondition)
		require.NoError(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", f.confirmed(state.Assignment)))

		require.NoError(t, f.createTask("prepared"))
	})

	t.Run("Given an issued assignment, when another Actor or altered bytes complete it, assert that the report is refused and the preparation stays pending", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)
		_, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		state, err := f.client.IssueWorkspacePreparation(f.ctx, f.session, "actor-uid")
		require.NoError(t, err)
		altered := f.confirmed(state.Assignment)
		altered.Assignment.ConfigDigest = strings.Repeat("f", 64)

		require.ErrorIs(t, f.client.CompleteWorkspacePreparation(f.ctx, "other-uid", f.confirmed(state.Assignment)), ErrFailedPrecondition)
		require.ErrorIs(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", altered), ErrFailedPrecondition)
		receipt, err := f.client.GetWorkspacePreparationReceipt(f.ctx, f.session.GetId())
		require.NoError(t, err)
		require.Equal(t, PreparationPending, receipt.GetClassification())
	})

	t.Run("Given a settled preparation, when the runtime repeats and then contradicts its report, assert that the repeat is accepted and the contradiction conflicts", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)
		_, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		state, err := f.client.IssueWorkspacePreparation(f.ctx, f.session, "actor-uid")
		require.NoError(t, err)
		report := f.confirmed(state.Assignment)
		require.NoError(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", report))

		require.NoError(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", report))
		failed := proto.CloneOf(report)
		failed.Confirmed = false
		require.ErrorIs(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", failed), ErrConflict)
	})

	t.Run("Given an unissued assignment, when the runtime reports it, assert that the report is refused", func(t *testing.T) {
		f := newPreparationFixture(t, preparationConfig)
		pending, err := f.client.BeginWorkspacePreparation(f.ctx, f.request, f.assignment, f.session)
		require.NoError(t, err)
		assignment := proto.CloneOf(f.assignment)
		assignment.RequestDigest, assignment.ExecutionId, assignment.ChallengeId = pending.GetRequestDigest(), pending.GetExecutionId(), pending.GetChallengeId()

		require.ErrorIs(t, f.client.CompleteWorkspacePreparation(f.ctx, "actor-uid", f.confirmed(assignment)), ErrFailedPrecondition)
	})
}

type preparationFixture struct {
	t          *testing.T
	ctx        context.Context
	client     *Client
	session    *apiv1.Session
	request    *apiv1.PrepareSessionWorkspaceRequest
	assignment *runtimev1.WorkspacePreparation
}

// newPreparationFixture creates a READY composed Session with a workspace on a
// variant whose published bootstrap config is config.
func newPreparationFixture(t *testing.T, config string) *preparationFixture {
	t.Helper()
	client, ctx := NewClient(setupTestDB(t), "public"), t.Context()
	sessionFixture(t, client, ctx, "team-a", "base", "assistant", "claude")
	variant := strings.Repeat("b", 64)
	selected := &RevisionComposition{
		Environment: &apiv1.DevelopmentEnvironment{Image: "registry.example/dev@sha256:" + strings.Repeat("1", 64), Platform: "linux/arm64", PolicyIdentity: "project/v1"},
		Runtime:     &apiv1.RuntimeComposition{PayloadImage: "registry.example/runtime@sha256:" + strings.Repeat("2", 64), Provider: "claude", Schema: 1, CliVersion: "2.1.285"},
	}
	bundle, err := bootstrap.NewBundle(variant, []byte(config), []byte(`{"name":"assistant"}`))
	require.NoError(t, err)
	require.NoError(t, recordErr(client.RecordRuntimeRevision(ctx, RuntimeRevision{
		Revision: variant, Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid",
		SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{}, EgressDestinations: []string{},
		ActorTemplateAtespace: "team-a", ActorTemplateName: "variant-template", ActorTemplateUID: "variant-template-uid",
		GitOrigins: []string{"github.com"}, BaseRevision: "base", Composition: selected,
	}, RecordRevisionOptions{Ready: true, Bootstrap: bundle})))
	request := newSessionRequest(uuid.NewString(), "assistant", "claude", "")
	request.PreparedRevision, request.DevelopmentEnvironment, request.RuntimeComposition = variant, selected.Environment, selected.Runtime
	request.Workspace = &apiv1.Workspace{Repo: "https://github.com/acme/app", Ref: strings.Repeat("a", 40), Branch: "main"}
	created, _, err := client.CreateSession(ctx, request, "create-request")
	require.NoError(t, err)
	create, err := client.BeginSessionOperation(ctx, created.GetId(), apiv1.RuntimeOperation_RUNTIME_OPERATION_CREATE, false)
	require.NoError(t, err)
	executor := uuid.New()
	_, err = client.ClaimSessionOperation(ctx, created.GetId(), create.ID, executor)
	require.NoError(t, err)
	session, err := client.FinishSessionOperation(ctx, created.GetId(), create.ID, executor, "runtime.example", "actor-uid", "")
	require.NoError(t, err)
	return &preparationFixture{
		t: t, ctx: ctx, client: client, session: session,
		request: &apiv1.PrepareSessionWorkspaceRequest{
			SessionId: session.GetId(), ActionId: "action-1", CreateRequestId: "create-request",
			GenerationId: uuid.NewString(), ActorUid: "actor-uid", PreparedRevision: variant,
			Workspace: session.GetWorkspace(), DevelopmentEnvironment: session.GetDevelopmentEnvironment(), RuntimeComposition: session.GetRuntimeComposition(),
			SetupProfile: "agent", SetupDigest: strings.Repeat("c", 64),
		},
		assignment: &runtimev1.WorkspacePreparation{
			SessionId: session.GetId(), ContextId: session.GetContextId(), ActionId: "action-1", CreateRequestId: "create-request", ActorUid: "actor-uid", Atespace: "team-a", ActorName: "session-" + session.GetId(),
			PreparedRevision: variant, Workspace: session.GetWorkspace(), Profile: "agent", SetupDigest: strings.Repeat("c", 64),
			ConfigDigest: strings.Repeat("d", 64), McpDigest: strings.Repeat("e", 64),
		},
	}
}

func (f *preparationFixture) confirmed(assignment *runtimev1.WorkspacePreparation) *runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest {
	return &runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest{
		SessionId: f.session.GetId(), Assignment: proto.CloneOf(assignment), Head: strings.Repeat("a", 40), Branch: "main",
		TransportDigest: strings.Repeat("9", 64), NativeHook: "append_system_prompt", ObservedAt: timestamppb.New(time.Now()), Confirmed: true,
	}
}

func (f *preparationFixture) createTask(id string) error {
	task := newSessionTask(id, id+"-message")
	task.ContextID = f.session.GetContextId()
	_, err := f.client.CreateRuntimeTask(f.ctx, f.session.GetId(), taskMutationHash(id), task, "")
	return err
}
