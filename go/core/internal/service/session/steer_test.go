package session

import (
	"context"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
)

// steerTestStore answers the steer's message lookup as accepted or unknown.
type steerTestStore struct {
	*interactionTestStore
	accepted bool
}

func (s *steerTestStore) GetSessionTaskByMessage(ctx context.Context, sessionID, taskID, messageID string) (*a2atype.Task, error) {
	if !s.accepted {
		return nil, database.ErrNotFound
	}
	return s.interactionTestStore.GetSessionTaskByMessage(ctx, sessionID, taskID, messageID)
}

func TestPrepareSteer(t *testing.T) {
	t.Run("Given a working task, when a steer names it, assert that it is prepared for the runtime without reserving a dispatch", func(t *testing.T) {
		f := newSteerServiceFixture(t, a2atype.TaskStateWorking, false)

		prepared, err := f.WhenSteered(f.steer("task"))

		require.NoError(t, err)
		require.Nil(t, prepared.AcceptedTask)
		require.Equal(t, f.sessionID, prepared.Session.Id)
		require.Empty(t, f.store.reservedIDs, "a steer must not reserve the Session's dispatch")
	})

	t.Run("Given a task that already accepted the steer, when it is sent again, assert that the accepted task is returned", func(t *testing.T) {
		f := newSteerServiceFixture(t, a2atype.TaskStateCompleted, true)

		prepared, err := f.WhenSteered(f.steer("task"))

		require.NoError(t, err)
		require.NotNil(t, prepared.AcceptedTask)
		require.Equal(t, a2atype.TaskID("task"), prepared.AcceptedTask.ID)
		require.Empty(t, f.store.reservedIDs)
	})

	for _, scenario := range []struct {
		state a2atype.TaskState
		want  string
	}{
		{state: a2atype.TaskStateCompleted, want: apia2a.SteerTaskNotRunning},
		{state: a2atype.TaskStateCanceled, want: apia2a.SteerTaskNotRunning},
		{state: a2atype.TaskStateFailed, want: apia2a.SteerTaskNotRunning},
		{state: a2atype.TaskStateInputRequired, want: apia2a.SteerInputRequired},
	} {
		t.Run("Given a "+string(scenario.state)+" task, when a steer names it, assert that it is refused as "+scenario.want, func(t *testing.T) {
			f := newSteerServiceFixture(t, scenario.state, false)

			_, err := f.WhenSteered(f.steer("task"))

			refusal, ok := apia2a.SteerRefusal(err)
			require.True(t, ok, "err = %v", err)
			require.Equal(t, scenario.want, refusal)
			require.Empty(t, f.store.reservedIDs)
		})
	}

	t.Run("Given a working task, when a steer names no task, assert that it is rejected as invalid", func(t *testing.T) {
		f := newSteerServiceFixture(t, a2atype.TaskStateWorking, false)

		_, err := f.WhenSteered(f.steer(""))

		require.ErrorIs(t, err, a2atype.ErrInvalidParams)
	})
}

type steerServiceFixture struct {
	t         *testing.T
	sessionID string
	agent     types.NamespacedName
	store     *steerTestStore
	service   *InteractionService
}

func newSteerServiceFixture(t *testing.T, state a2atype.TaskState, accepted bool) *steerServiceFixture {
	id := uuid.NewString()
	agent := types.NamespacedName{Namespace: "team-a", Name: "assistant"}
	stored := &apiv1.Session{Id: id, ContextId: id, State: apiv1.RuntimeState_RUNTIME_STATE_READY,
		Operation: apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE,
		Agent:     &apiv1.AgentProvenance{Ref: &apiv1.ResourceReference{Namespace: agent.Namespace, Name: agent.Name}}}
	store := &steerTestStore{interactionTestStore: &interactionTestStore{task: &a2atype.Task{ID: "task", ContextID: id,
		Status: a2atype.TaskStatus{State: state}}}, accepted: accepted}
	service := NewInteractionService(store, nil, NewService(&serviceTestStore{getResult: stored}, serviceTestAuthorizer{}, nil), false)
	return &steerServiceFixture{t: t, sessionID: id, agent: agent, store: store, service: service}
}

func (f *steerServiceFixture) steer(taskID a2atype.TaskID) *a2atype.SendMessageRequest {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("use main"))
	message.ContextID, message.TaskID = f.sessionID, taskID
	message.SetMeta(apia2a.SteerMetadataKey, true)
	return &a2atype.SendMessageRequest{Message: message}
}

func (f *steerServiceFixture) WhenSteered(req *a2atype.SendMessageRequest) (*PreparedSteer, error) {
	return f.service.PrepareSteer(serviceTestContext("alice"), f.agent, req)
}
