// Copyright 2026 The kagent Authors
// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"bytes"
	"context"
	"embed"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed mocks/invoke_claude_git_workspace.json
var gitWorkspaceMocks embed.FS

// The Git workspace tests clone a small public repository through the real
// egress gateway, so the cluster must reach github.com. The runtime does not
// trust a test-local HTTPS server, so there is no offline substitute.
const (
	gitWorkspaceRepo   = "https://github.com/octocat/Hello-World"
	gitWorkspaceOrigin = "github.com"
)

// The failure the bootstrap reports for a .git it did not create.
const foreignGitFailure = "already has a Git repository that the bootstrap did not create"

var gitCommitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestGitWorkspaceFreshSession verifies that a Session created with a Git
// workspace has the checkout in place when its first turn runs, that the
// bootstrap records completion in the state directory outside the workspace,
// and that the bootstrap leaves nothing of its own inside .git or the work tree.
func TestGitWorkspaceFreshSession(t *testing.T) {
	t.Parallel()
	fixture, _ := newGitWorkspaceFixture(t)

	session, err := fixture.sessions.GetSession(fixture.ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	require.Equal(t, gitWorkspaceRepo, session.GetSession().GetWorkspace().GetRepo(), "Session records the requested workspace")

	probe := probeWorkspace(t, fixture, "WS_PROBE_ONE: report the workspace state.", "toolu_ws_probe_one")
	require.Equal(t, "yes", probe.value("GIT_DIR"), "the first turn sees the checkout")
	require.Regexp(t, gitCommitID, probe.value("HEAD"), "the checkout has a commit")
	require.Contains(t, probe.files, "README", "the repository's files are checked out")
	require.Equal(t, "yes", probe.value("DONE"), "the done marker exists")
	require.Equal(t, "no", probe.value("STARTED"), "the started marker is gone once the checkout is complete")
	require.Equal(t, "0", probe.value("GIT_MARKERS"), "nothing bootstrap-related is written inside .git")
	require.Equal(t, "0", probe.value("STATUS"), "the markers do not show up as untracked files")
	require.Contains(t, probe.doneBody, "repo="+gitWorkspaceRepo)
}

// TestGitWorkspaceResumeKeepsCheckout verifies that resuming a suspended
// Session neither clones again nor loses what the agent wrote. A file inside
// .git proves the repository itself was not recreated.
func TestGitWorkspaceResumeKeepsCheckout(t *testing.T) {
	t.Parallel()
	fixture, _ := newGitWorkspaceFixture(t)

	before := probeWorkspace(t, fixture, "WS_PROBE_ONE: report the workspace state.", "toolu_ws_probe_one")
	require.Equal(t, "yes", before.value("DONE"))
	writeTurn := sendStreaming(t, fixture, "WS_WRITE_FILE: write a note and a sentinel.")
	require.Equal(t, a2atype.TaskStateCompleted, writeTurn.state, "write turn failed: %s", writeTurn.failureText)

	suspendWhenSettled(t, fixture)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, getSessionState(t, fixture).GetState())
	resumed, err := fixture.sessions.ResumeSession(fixture.ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.GetSession().GetState())

	after := probeWorkspace(t, fixture, "WS_PROBE_TWO: report the workspace state again.", "toolu_ws_probe_two")
	require.Equal(t, "yes", after.value("GIT_DIR"))
	require.Equal(t, before.value("HEAD"), after.value("HEAD"), "the checkout is the one made before the suspend")
	require.Equal(t, before.doneBody, after.doneBody, "the done marker was not rewritten, so the bootstrap did not run again")
	require.Equal(t, "yes", after.value("DONE"))
	require.Equal(t, "no", after.value("STARTED"))
	require.Equal(t, "KEEP_ME", after.value("NOTE"), "the agent's file survived the suspend")
	require.Equal(t, "SENTINEL", after.value("SENTINEL"), "the .git directory was not recreated")
	require.Equal(t, "0", after.value("GIT_MARKERS"))
}

// TestGitWorkspaceForeignGitFailsTurn verifies the bootstrap's refusal to
// touch a Git repository it did not create. The agent replaces .git and the
// done marker goes missing; every later turn then fails with the foreign-Git
// message before it reaches the model. The turn never recovers by deleting the
// repository and cloning again, which would end the failures and write the done
// marker.
func TestGitWorkspaceForeignGitFailsTurn(t *testing.T) {
	t.Parallel()
	fixture, model := newGitWorkspaceFixture(t)

	replace := sendStreaming(t, fixture, "WS_FOREIGN_REPLACE: replace the repository.")
	require.Equal(t, a2atype.TaskStateCompleted, replace.state, "replace turn failed: %s", replace.failureText)
	result := taskToolResults(getTask(t, fixture, replace.taskID))["toolu_ws_foreign"]
	require.Contains(t, result, "FOREIGN_READY", "the agent replaced .git and removed the markers")

	for attempt := 1; attempt <= 2; attempt++ {
		failed := sendStreaming(t, fixture, "WS_AFTER_FOREIGN: continue the work.")
		require.Equalf(t, a2atype.TaskStateFailed, failed.state, "attempt %d: turn after the foreign .git ended in %s, text %q", attempt, failed.state, failed.text)
		message := failed.failureText
		if message == "" {
			message = taskText(getTask(t, fixture, failed.taskID))
		}
		require.Containsf(t, message, foreignGitFailure, "attempt %d", attempt)
		require.Contains(t, message, "left untouched")
	}
	require.Zero(t, model.countContaining("WS_AFTER_FOREIGN"), "a failed bootstrap must not reach the model")
}

// TestSuspendSessionRacingSendMessage issues SuspendSession and SendMessage
// together. Either order is valid because a send does not resume a suspended
// Session: the suspend wins and the send is refused, or the send wins and the
// suspend is refused until the turn ends. In every case the Session must settle
// with no lifecycle operation left pending, hold only completed tasks, and run
// the next turn.
func TestSuspendSessionRacingSendMessage(t *testing.T) {
	t.Parallel()
	fixture, _ := newTurnFixture(t, "")

	_, request := newMessageRequest(t, "RACE_TURN: reply briefly.")
	request.Tenant, request.Message.ContextId = fixture.tenant, fixture.sessionID
	type sendOutcome struct {
		response *a2apb.SendMessageResponse
		err      error
	}
	start := make(chan struct{})
	suspended := make(chan error, 1)
	sent := make(chan sendOutcome, 1)
	go func() {
		<-start
		_, err := fixture.sessions.SuspendSession(fixture.ctx, &apiv1alpha1.SuspendSessionRequest{SessionId: fixture.sessionID})
		suspended <- err
	}()
	go func() {
		<-start
		response, err := sendMessageWithRetry(fixture.ctx, fixture.client, request)
		sent <- sendOutcome{response, err}
	}()
	close(start)
	suspendErr, send := <-suspended, <-sent

	var completed []a2atype.TaskID
	suspendCode := status.Code(suspendErr)
	require.Containsf(t, []codes.Code{codes.OK, codes.Aborted, codes.FailedPrecondition}, suspendCode, "SuspendSession returned %v", suspendErr)
	if send.err == nil {
		task := decodeTask(t, send.response)
		require.Equal(t, a2atype.TaskStateCompleted, task.Status.State, "an accepted turn completes: %s", taskText(task))
		completed = append(completed, task.ID)
	} else {
		require.Equalf(t, codes.FailedPrecondition, status.Code(send.err), "a refused SendMessage is FailedPrecondition, got %v", send.err)
	}

	// The Session settles with no operation pending, and a suspend that
	// succeeded is the reason it is suspended.
	settled := waitForSettledSession(t, fixture)
	if suspendCode == codes.OK {
		require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, settled.GetState(), "a successful suspend leaves the Session suspended")
	} else {
		require.Equalf(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, settled.GetState(), "a refused suspend (%v) leaves the Session ready", suspendErr)
	}
	if send.err != nil {
		require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, settled.GetState(), "a send is refused only because the Session was suspending")
	}

	// Recover as a client would: resume if needed, send again what was refused,
	// then prove the Session still takes new work.
	if settled.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED {
		resumed, err := fixture.sessions.ResumeSession(fixture.ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: fixture.sessionID})
		require.NoError(t, err)
		require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, resumed.GetSession().GetState())
	}
	if send.err != nil {
		_, _, task := fixture.send(t, "RACE_TURN: reply briefly.")
		require.Equal(t, a2atype.TaskStateCompleted, task.Status.State, "the resent turn completes: %s", taskText(task))
		completed = append(completed, task.ID)
	}
	_, _, followUp := fixture.send(t, "FOLLOW_UP_TURN: reply briefly.")
	require.Equal(t, a2atype.TaskStateCompleted, followUp.Status.State, "the next turn completes: %s", taskText(followUp))
	require.Contains(t, taskText(followUp), "FOLLOW_UP_TURN_DONE")
	completed = append(completed, followUp.ID)

	assertTaskHistory(t, fixture, completed...)
	final := waitForSettledSession(t, fixture)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, final.GetState())
}

// TestSuspendSessionRefusedWhileTurnInFlight pins the behavior of suspending a
// Session whose turn is running: the control plane refuses with Aborted ("the
// session has an active task") instead of interrupting the turn. The turn
// completes untouched, and the Session can be suspended once the turn is over.
// The model is held until the suspend has been refused, so the turn is in
// flight, with its task past the dispatch lease, for the whole attempt.
func TestSuspendSessionRefusedWhileTurnInFlight(t *testing.T) {
	t.Parallel()
	fixture, model := newTurnFixture(t, "GATED_TURN")

	_, request := newMessageRequest(t, "GATED_TURN: reply briefly.")
	request.Tenant, request.Message.ContextId = fixture.tenant, fixture.sessionID
	type sendOutcome struct {
		response *a2apb.SendMessageResponse
		err      error
	}
	sent := make(chan sendOutcome, 1)
	go func() {
		response, err := sendMessageWithRetry(fixture.ctx, fixture.client, request)
		sent <- sendOutcome{response, err}
	}()
	select {
	case <-model.started:
	case <-fixture.ctx.Done():
		t.Fatal("the turn never reached the model")
	}

	_, err := fixture.sessions.SuspendSession(fixture.ctx, &apiv1alpha1.SuspendSessionRequest{SessionId: fixture.sessionID})
	require.Equalf(t, codes.Aborted, status.Code(err), "SuspendSession during a running turn = %v, want Aborted", err)
	require.Contains(t, status.Convert(err).Message(), "active task")
	current := getSessionState(t, fixture)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, current.GetState(), "a refused suspend leaves the Session ready")
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.GetOperation(), "a refused suspend starts no operation")

	model.release()
	var send sendOutcome
	select {
	case send = <-sent:
	case <-fixture.ctx.Done():
		t.Fatal("the turn did not finish after the model was released")
	}
	require.NoError(t, send.err)
	task := decodeTask(t, send.response)
	require.Equal(t, a2atype.TaskStateCompleted, task.Status.State, "the refused suspend must not disturb the turn: %s", taskText(task))
	require.Contains(t, taskText(task), "GATED_TURN_DONE")

	suspendWhenSettled(t, fixture)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, getSessionState(t, fixture).GetState())
	_, err = fixture.sessions.ResumeSession(fixture.ctx, &apiv1alpha1.ResumeSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	_, _, followUp := fixture.send(t, "FOLLOW_UP_TURN: reply briefly.")
	require.Equal(t, a2atype.TaskStateCompleted, followUp.Status.State, "the Session takes new work after the suspend and resume: %s", taskText(followUp))
	assertTaskHistory(t, fixture, task.ID, followUp.ID)
}

// workspaceProbe is the parsed output of the mock agent's state-probe command:
// KEY=value lines, FILE:<name> lines for the workspace listing and DONE_BODY:
// lines for the done marker.
type workspaceProbe struct {
	values   map[string]string
	files    []string
	doneBody string
}

func (p workspaceProbe) value(key string) string { return p.values[key] }

func parseWorkspaceProbe(output string) workspaceProbe {
	probe := workspaceProbe{values: map[string]string{}}
	var body []string
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "FILE:"):
			probe.files = append(probe.files, strings.TrimPrefix(line, "FILE:"))
		case strings.HasPrefix(line, "DONE_BODY:"):
			body = append(body, strings.TrimPrefix(line, "DONE_BODY:"))
		default:
			if key, value, ok := strings.Cut(line, "="); ok && key != "" && !strings.ContainsAny(key, " \t") {
				probe.values[key] = value
			}
		}
	}
	probe.doneBody = strings.Join(body, "\n")
	return probe
}

// probeWorkspace runs a turn whose mock Bash call prints the workspace state,
// and returns what the tool reported. The state is read from the persisted
// task's tool result, since the mock model cannot match on output content.
func probeWorkspace(t *testing.T, fixture *interactionFixture, prompt, toolUseID string) workspaceProbe {
	t.Helper()
	streamed := sendStreaming(t, fixture, prompt)
	require.Equalf(t, a2atype.TaskStateCompleted, streamed.state, "probe turn %q failed: %s", prompt, streamed.failureText)
	output, ok := taskToolResults(getTask(t, fixture, streamed.taskID))[toolUseID]
	require.Truef(t, ok, "task has no result for tool call %s", toolUseID)
	return parseWorkspaceProbe(output)
}

// taskToolResults returns the text each tool call of the task reported, by
// tool-use ID.
func taskToolResults(task *a2atype.Task) map[string]string {
	results := map[string]string{}
	collect := func(parts []*a2atype.Part) {
		for _, part := range parts {
			if partType, _ := part.Metadata[apia2a.PartTypeMetadataKey].(string); partType != "function_response" {
				continue
			}
			data, ok := part.Data().(map[string]any)
			if !ok {
				continue
			}
			id, _ := data["id"].(string)
			response, _ := data["response"].(map[string]any)
			if result, ok := response["result"].(string); ok && id != "" {
				results[id] = result
			}
		}
	}
	for _, message := range task.History {
		if message != nil {
			collect(message.Parts)
		}
	}
	if task.Status.Message != nil {
		collect(task.Status.Message.Parts)
	}
	for _, artifact := range task.Artifacts {
		if artifact != nil {
			collect(artifact.Parts)
		}
	}
	return results
}

func decodeTask(t *testing.T, response *a2apb.SendMessageResponse) *a2atype.Task {
	t.Helper()
	result, err := pbconv.FromProtoSendMessageResponse(response)
	require.NoError(t, err)
	task, ok := result.(*a2atype.Task)
	require.Truef(t, ok, "A2A response = %T, want Task", result)
	return task
}

func getSessionState(t *testing.T, fixture *interactionFixture) *apiv1alpha1.Session {
	t.Helper()
	response, err := fixture.sessions.GetSession(fixture.ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
	require.NoError(t, err)
	return response.GetSession()
}

// waitForSettledSession waits until no lifecycle operation is pending.
func waitForSettledSession(t *testing.T, fixture *interactionFixture) *apiv1alpha1.Session {
	t.Helper()
	var session *apiv1alpha1.Session
	err := wait.PollUntilContextTimeout(fixture.ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		response, err := fixture.sessions.GetSession(ctx, &apiv1alpha1.GetSessionRequest{SessionId: fixture.sessionID})
		if err != nil {
			return false, err
		}
		session = response.GetSession()
		return session.GetOperation() == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE ||
			session.GetOperation() == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_UNSPECIFIED, nil
	})
	require.NoErrorf(t, err, "Session never settled; last state %v", session)
	return session
}

// suspendWhenSettled suspends the Session once the previous turn's lifecycle
// work is over. Completion is public before the post-turn snapshot finishes, so
// a suspend issued right after a turn may be refused until then.
func suspendWhenSettled(t *testing.T, fixture *interactionFixture) {
	t.Helper()
	err := wait.PollUntilContextTimeout(fixture.ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := fixture.sessions.SuspendSession(ctx, &apiv1alpha1.SuspendSessionRequest{SessionId: fixture.sessionID})
		switch status.Code(err) {
		case codes.OK:
			return true, nil
		case codes.Aborted, codes.FailedPrecondition:
			return false, nil
		default:
			return false, err
		}
	})
	require.NoError(t, err, "suspend Session")
}

// newGitWorkspaceFixture creates a Claude Harness that may clone from GitHub, an
// Agent on it backed by the Git workspace mock model, and a Session requesting
// a shallow checkout of the public test repository.
func newGitWorkspaceFixture(t *testing.T) (*interactionFixture, *gatedModelProxy) {
	t.Helper()
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	model := startGatedModelProxy(t, startMockLLMServer(t, gitWorkspaceMocks, "mocks/invoke_claude_git_workspace.json"), "")
	modelConfig := createClaudeMockModel(t, kube, reachableServerURL(t, model.URL, ""))
	harness := createGitWorkspaceHarness(t, kube)
	template := createGitWorkspaceTemplate(t, kube, harness.Name, modelConfig.Name)
	fixture := newInteractionFixtureWithWorkspace(t, target, template, &apiv1alpha1.Workspace{Repo: gitWorkspaceRepo, Depth: 1})
	return fixture, model
}

// newTurnFixture creates a plain Claude Session, without a workspace, backed by
// the same mock model. A non-empty gate holds the first model request that
// carries it until the test releases it.
func newTurnFixture(t *testing.T, gate string) (*interactionFixture, *gatedModelProxy) {
	t.Helper()
	target := interactionTarget(t)
	model := startGatedModelProxy(t, startMockLLMServer(t, gitWorkspaceMocks, "mocks/invoke_claude_git_workspace.json"), gate)
	templateName := createClaudeMockTemplate(t, reachableServerURL(t, model.URL, ""))
	return newInteractionFixtureForHarnessTemplate(t, target, claudeE2EHarness, templateName), model
}

// createGitWorkspaceHarness clones the suite's Claude Harness and lets its
// Sessions clone from GitHub, anonymously.
func createGitWorkspaceHarness(t *testing.T, kube ctrlclient.Client) *v1alpha3.Harness {
	t.Helper()
	base := &v1alpha3.Harness{}
	if err := kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: claudeE2EHarness}, base); err != nil {
		t.Fatalf("get %s Harness: %v", claudeE2EHarness, err)
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "claude-git-", Namespace: "kagent"},
		Spec:       *base.Spec.DeepCopy(),
	}
	harness.Spec.Git = &v1alpha3.HarnessGit{Origins: []string{gitWorkspaceOrigin}}
	if err := kube.Create(t.Context(), harness); err != nil {
		t.Fatalf("create Git workspace Harness: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), harness); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Git workspace Harness: %v", err)
		}
	})
	return harness
}

func createGitWorkspaceTemplate(t *testing.T, kube ctrlclient.Client, harnessName, modelConfig string) string {
	t.Helper()
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "claude-git-", Namespace: "kagent",
			Labels: map[string]string{"kagent.dev/e2e-runtime": "claude", "kagent.dev/harness": harnessName},
		},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig:  &corev1.LocalObjectReference{Name: modelConfig},
			Description:  "Claude Git workspace E2E fixture",
			SystemPrompt: "Follow the requested steps exactly.",
		},
	}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harnessName)
	return template.Name
}

// gatedModelProxy fronts the mock LLM. It keeps each request body so a test can
// tell whether the model was called, and it can hold the first request that
// contains the gate text until release is called.
type gatedModelProxy struct {
	// URL is the proxy's listener on the test host.
	URL string
	// started is closed when the gated request arrives.
	started chan struct{}

	gate        string
	gateOnce    sync.Once
	releaseOnce sync.Once
	released    chan struct{}

	mu       sync.Mutex
	requests [][]byte
}

func startGatedModelProxy(t *testing.T, upstreamURL, gate string) *gatedModelProxy {
	t.Helper()
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &gatedModelProxy{gate: gate, started: make(chan struct{}), released: make(chan struct{})}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.FlushInterval = -1
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		proxy.mu.Lock()
		proxy.requests = append(proxy.requests, body)
		proxy.mu.Unlock()
		if proxy.gate != "" && bytes.Contains(body, []byte(proxy.gate)) {
			held := false
			proxy.gateOnce.Do(func() {
				held = true
				close(proxy.started)
			})
			if held {
				select {
				case <-proxy.released:
				case <-r.Context().Done():
					return
				}
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		reverse.ServeHTTP(w, r)
	}))
	_ = server.Listener.Close()
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() {
		proxy.release()
		server.Close()
	})
	proxy.URL = server.URL
	return proxy
}

func (p *gatedModelProxy) release() { p.releaseOnce.Do(func() { close(p.released) }) }

// countContaining returns how many model requests carried the text.
func (p *gatedModelProxy) countContaining(text string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, body := range p.requests {
		if bytes.Contains(body, []byte(text)) {
			count++
		}
	}
	return count
}
