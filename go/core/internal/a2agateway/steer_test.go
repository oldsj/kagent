package a2agateway

import (
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
)

func TestGatewaySteer(t *testing.T) {
	t.Run("Given a working task, when a steer names it, assert that the runtime receives it without a dispatch and the running task is returned", func(t *testing.T) {
		f := newSteerGatewayFixture(t)

		result, err := f.gateway.SendMessage(gatewayTestContext(), f.steer())

		require.NoError(t, err)
		require.Equal(t, f.current.ID, result.(*a2atype.Task).ID)
		require.Equal(t, 1, f.runtime.sendCalls)
		require.Equal(t, f.current.ID, f.runtime.sentTaskID)
		require.True(t, f.runtime.sentConfig.ReturnImmediately, "a steer must not wait for its task to end")
		require.Zero(t, f.store.reserveCalls, "a steer must not reserve the Session's dispatch")
	})

	t.Run("Given a working task, when a steer is streamed, assert that the stream carries only the running task", func(t *testing.T) {
		f := newSteerGatewayFixture(t)

		var events []a2atype.Event
		for event, err := range f.gateway.SendStreamingMessage(gatewayTestContext(), f.steer()) {
			require.NoError(t, err)
			events = append(events, event)
		}

		require.Len(t, events, 1)
		require.Equal(t, f.current.ID, events[0].(*a2atype.Task).ID)
		require.Zero(t, f.store.reserveCalls)
	})

	t.Run("Given a runtime that refuses the steer, when a steer arrives, assert that the caller receives the refusal reason", func(t *testing.T) {
		f := newSteerGatewayFixture(t)
		f.runtime.onSend = func() error { return apia2a.SteerNotAccepted(apia2a.SteerUnsupported) }

		_, err := f.gateway.SendMessage(gatewayTestContext(), f.steer())

		refusal, ok := apia2a.SteerRefusal(err)
		require.True(t, ok, "err = %v", err)
		require.Equal(t, apia2a.SteerUnsupported, refusal)
	})

	t.Run("Given a task that already accepted the steer, when it is sent again, assert that the task is returned without contacting the runtime", func(t *testing.T) {
		f := newSteerGatewayFixture(t)
		f.store.replay = f.current

		result, err := f.gateway.SendMessage(gatewayTestContext(), f.steer())

		require.NoError(t, err)
		require.Equal(t, f.current.ID, result.(*a2atype.Task).ID)
		require.Zero(t, f.runtime.sendCalls)
	})

	t.Run("Given a completed task, when a steer names it, assert that it is refused without contacting the runtime", func(t *testing.T) {
		f := newSteerGatewayFixture(t)
		f.current.Status.State = a2atype.TaskStateCompleted

		_, err := f.gateway.SendMessage(gatewayTestContext(), f.steer())

		refusal, ok := apia2a.SteerRefusal(err)
		require.True(t, ok, "err = %v", err)
		require.Equal(t, apia2a.SteerTaskNotRunning, refusal)
		require.Zero(t, f.runtime.sendCalls)
	})
}

type steerGatewayFixture struct {
	current *a2atype.Task
	store   *gatewayTestStore
	runtime *gatewayTestRuntime
	gateway a2asrv.RequestHandler
}

func newSteerGatewayFixture(t *testing.T) *steerGatewayFixture {
	session := gatewayTestSession()
	current := &a2atype.Task{ID: "task", ContextID: session.ContextId, Status: a2atype.TaskStatus{State: a2atype.TaskStateWorking}}
	store := &gatewayTestStore{session: session, task: current}
	runtime := &gatewayTestRuntime{task: current}
	gateway := newTestGateway(store, &gatewayTestAuthorizer{}, &gatewayTestDialer{client: gatewayTestClient(t, runtime)}, gatewayTestURL)
	return &steerGatewayFixture{current: current, store: store, runtime: runtime, gateway: gateway}
}

func (f *steerGatewayFixture) steer() *a2atype.SendMessageRequest {
	request := gatewayTestRequest()
	request.Message.TaskID = f.current.ID
	request.Message.ContextID = ""
	request.Message.SetMeta(apia2a.SteerMetadataKey, true)
	return request
}
