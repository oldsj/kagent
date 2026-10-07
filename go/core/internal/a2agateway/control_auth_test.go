package a2agateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/internal/service/kubecrud"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHTTPServiceAuthAndScope(t *testing.T) {
	token := strings.Repeat("s", 32)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(token), 0600))
	policy, err := controlauth.New(controlauth.Config{Namespace: "team-a", Agents: []string{"assistant"}})
	require.NoError(t, err)
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	session := gatewayTestSession()
	session.Creator = "mainloop"
	store := &gatewayTestStore{session: session, task: &a2a.Task{ID: "task", ContextID: gatewayTestContextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}}
	dialer := &gatewayTestDialer{}
	handler := NewHTTPHandler(newTestGateway(store, policy, dialer, ""), provider, nil)
	for _, tc := range []struct {
		name, path, method, params string
		headers                    []string
		denied                     bool
	}{
		{"missing", gatewayTestAgent, "GetTask", `{"id":"task"}`, nil, true},
		{"bad", gatewayTestAgent, "GetTask", `{"id":"task"}`, []string{"Bearer " + strings.Repeat("b", 32)}, true},
		{"duplicate", gatewayTestAgent, "GetTask", `{"id":"task"}`, []string{"Bearer " + token, "Bearer " + token}, true},
		{"other Agent", "team-a/other", "GetTask", `{"id":"task"}`, []string{"Bearer " + token}, true},
		{"forbidden method", gatewayTestAgent, "SendMessage", `{"message":{"messageId":"test","role":"ROLE_USER","contextId":"` + gatewayTestID + `","parts":[{"text":"hello"}]}}`, []string{"Bearer " + token}, true},
		{"implicit Session", gatewayTestAgent, "SendStreamingMessage", `{"message":{"messageId":"test","role":"ROLE_USER","parts":[{"text":"hello"}]}}`, []string{"Bearer " + token}, true},
		{"allowed", gatewayTestAgent, "GetTask", `{"id":"task"}`, []string{"Bearer " + token}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":"req","method":"` + tc.method + `","params":` + tc.params + `}`
			request := httptest.NewRequest(http.MethodPost, "http://controller"+HTTPPathPrefix+tc.path+"?user_id=mainloop", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-User-Id", "mainloop")
			request.Header.Set("X-Agent-Name", "team-a/assistant")
			for _, value := range tc.headers {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if tc.denied {
				if len(tc.headers) != 1 || tc.name == "bad" {
					require.Equal(t, 401, response.Code)
				} else {
					require.Contains(t, response.Body.String(), `"code":-31403`)
				}
				require.Zero(t, store.reserveCalls)
				require.Nil(t, store.created)
			} else {
				require.Equal(t, 200, response.Code)
				require.NotContains(t, response.Body.String(), `"error"`)
				require.Contains(t, response.Body.String(), `"id":"task"`)
			}
			require.Nil(t, dialer.session)
		})
	}
	// Denied ownership never grants runtime dispatch.
	session.Creator = "other"
	request := httptest.NewRequest(http.MethodPost, "http://controller"+HTTPPathPrefix+gatewayTestAgent, strings.NewReader(`{"jsonrpc":"2.0","id":"r","method":"GetTask","params":{"id":"task"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Contains(t, response.Body.String(), `"code":-31403`)
	require.Nil(t, dialer.session)
}

func TestServiceBearerRemovedFromRuntimeParams(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "team-a", Agents: []string{"assistant"}})
	require.NoError(t, err)
	token := strings.Repeat("s", 32)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(token), 0600))
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	session, err := provider.Authenticate(t.Context(), http.Header{"Authorization": {"Bearer " + token}}, nil)
	require.NoError(t, err)
	interceptor := &upstreamAuthInterceptor{authenticator: provider, session: &api.Session{Id: "session"}, targetActor: "team-a/actor"}
	req := &a2aclient.Request{BaseURL: "runtime", ServiceParams: a2aclient.ServiceParams{"authorization": {"Bearer " + token}, "Authorization": {"Bearer " + token}}}
	_, _, err = interceptor.Before(auth.AuthSessionTo(t.Context(), session), req)
	require.NoError(t, err)
	for key := range req.ServiceParams {
		require.False(t, strings.EqualFold(key, "authorization"))
	}
	require.Equal(t, []string{"mainloop"}, req.ServiceParams["x-user-id"])
}

func TestServiceA2AAllowedOperations(t *testing.T) {
	token := strings.Repeat("s", 32)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(token), 0600))
	policy, err := controlauth.New(controlauth.Config{Namespace: "team-a", Agents: []string{"assistant"}})
	require.NoError(t, err)
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	session := gatewayTestSession()
	session.Creator = "mainloop"
	task := &a2a.Task{ID: "task", ContextID: session.Id, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	store := &gatewayTestStore{session: session, task: task, tasks: []*a2a.Task{task}, total: 1, replay: task}
	runtime := &gatewayTestRuntime{task: task}

	scheme := k8sruntime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	agent := &v1alpha3.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant"}, Status: v1alpha3.AgentStatus{LatestSuccessfulRevision: "revision-1"}}
	agents := kubecrud.NewService(fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build(), policy, &v1alpha3.Agent{}, &v1alpha3.AgentList{}, "Agent")
	store.revision = &database.RuntimeRevision{AgentCard: &a2apb.AgentCard{Name: "assistant", Version: "1", SupportedInterfaces: []*a2apb.AgentInterface{{Url: "http://runtime", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"}}, Capabilities: &a2apb.AgentCapabilities{}, DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"}}}
	gateway := New(sessionsvc.NewInteractionService(store, agents, newTestSessions(store, policy)), &gatewayTestDialer{client: gatewayTestClient(t, runtime)}, "")
	server := httptest.NewServer(NewHTTPHandler(gateway, provider, nil))
	defer server.Close()
	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL+HTTPPathPrefix+gatewayTestAgent, a2a.TransportProtocolJSONRPC)})
	require.NoError(t, err)
	defer client.Destroy()
	ctx := a2aclient.AttachServiceParams(t.Context(), a2aclient.ServiceParams{"authorization": {"Bearer " + token}})

	card, err := client.GetExtendedAgentCard(ctx, &a2a.GetExtendedAgentCardRequest{})
	require.NoError(t, err)
	require.Equal(t, "assistant", card.Name)
	got, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: "task"})
	require.NoError(t, err)
	require.Equal(t, task.ID, got.ID)
	list, err := client.ListTasks(ctx, &a2a.ListTasksRequest{ContextID: session.Id})
	require.NoError(t, err)
	require.Len(t, list.Tasks, 1)
	_, err = client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "task"})
	require.NoError(t, err)
	events := 0
	for event, err := range client.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: "task"}) {
		require.NoError(t, err)
		require.NotNil(t, event)
		events++
	}
	require.Positive(t, events)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("continue"))
	message.ContextID = session.Id
	message.TaskID = "task"
	events = 0
	for event, err := range client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message}) {
		require.NoError(t, err)
		require.NotNil(t, event)
		events++
	}
	require.Positive(t, events)
	require.Equal(t, 1, store.reserveCalls)
}
