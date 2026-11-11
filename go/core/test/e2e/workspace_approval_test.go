package e2e_test

import (
	"bytes"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

// workspaceRepository is a small public repository. Its clone leaves the Actor
// through the egress gateway, as a Session's own repository does.
const workspaceRepository = "https://github.com/octocat/Hello-World"

// TestWorkspaceApprovalSurvivesSuspension covers the Session shape that has a
// Git workspace: its Harness enables Git, so the harness wraps its driver with
// the workspace bootstrap. A tool approval requested by such a Session must
// still be answerable after the Actor has been suspended to a DATA snapshot.
func TestWorkspaceApprovalSurvivesSuspension(t *testing.T) {
	t.Parallel()
	forEachHarness(t, func(t *testing.T, harness testHarness) {
		t.Parallel()
		if harness.name != codexE2EHarness && harness.name != claudeE2EHarness {
			t.Skip("spec.git is supported only by the Codex and Claude harnesses")
		}
		target := interactionTarget(t)
		harness = harness.withGit(t, interactionKubeClient(t), "github.com")
		mcpURL, mcpServer := startMCPMock(t)
		template, toolName := createMCPInteractionTemplate(t, harness, mcpURL, true, nil)
		fixture := newInteractionFixtureForRequest(t, target, &apiv1.CreateSessionRequest{
			Agent:     &apiv1.ResourceReference{Namespace: "kagent", Name: template},
			RequestId: uuid.NewString(),
			Workspace: &apiv1.Workspace{Repo: workspaceRepository, Depth: 1},
		})

		session, err := fixture.sessions.GetSession(fixture.ctx, &apiv1.GetSessionRequest{SessionId: fixture.sessionID})
		if err != nil || session.GetSession().GetWorkspace().GetRepo() != workspaceRepository {
			t.Fatalf("Session workspace = %+v, error %v; want %s", session.GetSession().GetWorkspace(), err, workspaceRepository)
		}

		// The first turn clones the workspace, then parks on the approval. The
		// helper waits for the Actor to be suspended before it answers.
		completed := sendToolDecision(t, fixture, "Add 3 and 5 using the configured MCP server.", toolName, true)

		if completed.Status.State != a2atype.TaskStateCompleted {
			t.Fatalf("task state after the approval = %s, text = %q; want COMPLETED", completed.Status.State, taskText(completed))
		}
		if !strings.Contains(taskText(completed), "result is 8") {
			t.Fatalf("approved MCP result = %q", taskText(completed))
		}
		calls := 0
		for _, request := range mcpServer.Requests() {
			if bytes.Contains(request.Body, []byte(`"method":"tools/call"`)) && bytes.Contains(request.Body, []byte(`"name":"add_numbers"`)) {
				calls++
			}
		}
		if calls != 1 {
			t.Fatalf("MCP executions = %d, want the approved call to run once", calls)
		}
	})
}
