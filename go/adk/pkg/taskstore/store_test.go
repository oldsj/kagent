package taskstore

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	sdktaskstore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// deniedUpdateServer refuses the first denials UpdateTask calls with
// PermissionDenied, as the TaskStore did while the controller restarted.
type deniedUpdateServer struct {
	apiv1alpha1.UnimplementedTaskStoreServiceServer
	mu      sync.Mutex
	denials int
	updates []recordedUpdate
	settled []int64
}

type recordedUpdate struct {
	state           string
	expectedVersion int64
}

func (s *deniedUpdateServer) recorded() ([]recordedUpdate, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedUpdate(nil), s.updates...), append([]int64(nil), s.settled...)
}

func (s *deniedUpdateServer) UpdateTask(_ context.Context, request *apiv1alpha1.TaskStoreServiceUpdateTaskRequest) (*apiv1alpha1.TaskStoreServiceUpdateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, recordedUpdate{
		state: request.Task.GetStatus().GetState().String(), expectedVersion: request.ExpectedVersion,
	})
	if s.denials != 0 {
		s.denials--
		return nil, status.Error(codes.PermissionDenied, "actor does not belong to this session")
	}
	return &apiv1alpha1.TaskStoreServiceUpdateTaskResponse{Version: request.ExpectedVersion + 1}, nil
}

func (s *deniedUpdateServer) SettleTask(_ context.Context, request *apiv1alpha1.TaskStoreServiceSettleTaskRequest) (*apiv1alpha1.TaskStoreServiceSettleTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled = append(s.settled, request.Version)
	return &apiv1alpha1.TaskStoreServiceSettleTaskResponse{}, nil
}

func newDeniedUpdateStore(t *testing.T, api *deniedUpdateServer, window time.Duration) *Store {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	apiv1alpha1.RegisterTaskStoreServiceServer(server, api)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := controllerclient.New(controllerclient.Config{
		APIURL: "http://api.test", DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	dir := t.TempDir()
	for field, value := range map[string]string{"name": "session-" + uuid.NewString() + "-0123456789abcdef", "atespace": "team-a", "uid": "actor-uid"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, field), []byte(value), 0o600))
	}
	store := New(client, filepath.Join(dir, "name"))
	store.failureRecordWindow = window
	return store
}

func taskUpdate(state a2a.TaskState, version int64) *sdktaskstore.UpdateRequest {
	task := &a2a.Task{ID: "task", ContextID: "conversation", Status: a2a.TaskStatus{State: state}}
	input := &a2asrv.ExecutorContext{TaskID: task.ID, ContextID: task.ContextID}
	return &sdktaskstore.UpdateRequest{
		Task: task, PrevVersion: sdktaskstore.TaskVersion(version),
		Event: a2a.NewStatusUpdateEvent(input, state, nil),
	}
}

func TestFailedSaveEndsTaskWithRetriedFailureRecord(t *testing.T) {
	// The working save and the first two failure-record attempts are refused.
	api := &deniedUpdateServer{denials: 3}
	store := newDeniedUpdateStore(t, api, 10*time.Second)
	wrapper := store.WrapExecutor(a2asrv.AgentExecutorFunc(nil), "", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = context.WithValue(ctx, executionKey{}, &execution{ready: make(chan struct{})})
	wrapper.track(ctx, "task")

	_, err := store.Update(ctx, taskUpdate(a2a.TaskStateWorking, 4))
	require.ErrorIs(t, err, a2a.ErrUnauthorized)
	// Later events from the same execution are not persisted.
	_, err = store.Update(ctx, taskUpdate(a2a.TaskStateWorking, 4))
	require.ErrorContains(t, err, "previous task persistence failed")
	updates, _ := api.recorded()
	require.Len(t, updates, 1)

	// The SDK's failure record is retried until the controller accepts it, even
	// though the execution context has already ended.
	cancel()
	version, err := store.Update(ctx, taskUpdate(a2a.TaskStateFailed, 4))
	require.NoError(t, err)
	require.EqualValues(t, 5, version)
	updates, _ = api.recorded()
	require.Len(t, updates, 4)
	for _, update := range updates[1:] {
		require.Equal(t, "TASK_STATE_FAILED", update.state)
		require.EqualValues(t, 4, update.expectedVersion, "the failure record must not advance past the last committed save")
	}

	// The failed task is then settled, so the Session can be suspended.
	wrapper.Cleanup(ctx, &a2asrv.ExecutorContext{TaskID: "task", ContextID: "conversation"}, nil, err)
	_, settled := api.recorded()
	require.Equal(t, []int64{5}, settled)
}

func TestFailureRecordRetryIsBounded(t *testing.T) {
	api := &deniedUpdateServer{denials: 1 << 30}
	store := newDeniedUpdateStore(t, api, 300*time.Millisecond)
	ctx := context.WithValue(t.Context(), executionKey{}, &execution{ready: make(chan struct{})})
	_, err := store.Update(ctx, taskUpdate(a2a.TaskStateWorking, 1))
	require.ErrorIs(t, err, a2a.ErrUnauthorized)

	started := time.Now()
	_, err = store.Update(ctx, taskUpdate(a2a.TaskStateFailed, 1))
	require.ErrorIs(t, err, a2a.ErrUnauthorized)
	require.Less(t, time.Since(started), 2*time.Second)
	updates, _ := api.recorded()
	require.Greater(t, len(updates), 2, "the failure record must be retried")
}
