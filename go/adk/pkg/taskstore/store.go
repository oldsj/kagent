// Package taskstore connects the upstream A2A SDK to kagent's private gRPC
// persistence service. It keeps no local copy of public conversation history.
package taskstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	sdktaskstore "github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const RuntimeTokenHeader = "x-kagent-runtime-token"
const RuntimeTokenPlaceholder = "gateway-injection-required"

type Store struct {
	client       *controllerclient.Client
	identityPath string
}

var _ sdktaskstore.Store = (*Store)(nil)

func New(client *controllerclient.Client, identityPath string) *Store {
	return &Store{client: client, identityPath: identityPath}
}

func (s *Store) sessionID() (string, error) {
	identity, err := os.ReadFile(s.identityPath)
	if err != nil {
		return "", fmt.Errorf("read runtime identity: %w", err)
	}
	name := strings.TrimSpace(string(identity))
	if len(name) != 61 || !strings.HasPrefix(name, "session-") || name[44] != '-' {
		return "", fmt.Errorf("unexpected runtime generation name")
	}
	id := name[8:44]
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return "", fmt.Errorf("invalid runtime session hint")
	}
	suffix, err := hex.DecodeString(name[45:])
	if err != nil || len(suffix) != 8 || hex.EncodeToString(suffix) != name[45:] {
		return "", fmt.Errorf("invalid runtime generation hint")
	}
	return id, nil
}

func (s *Store) callContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := s.client.CallContext(ctx, "")
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Delete("authorization")
	md.Delete("x-user-id")
	md.Delete("x-agent-name")
	md.Delete("x-share-token")
	// The gateway replaces this nonsecret placeholder. Unsigned projection
	// files supply routing hints only and are never proof of runtime authority.
	md.Delete(apia2a.InsecureRuntimeIdentityHeader)
	md.Set(RuntimeTokenHeader, RuntimeTokenPlaceholder)
	return metadata.NewOutgoingContext(ctx, md), cancel, nil
}

// Create persists the SDK's initial task and returns its committed version.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (sdktaskstore.TaskVersion, error) {
	wire, err := pbconv.ToProtoTask(task)
	if err != nil {
		return 0, err
	}
	id, err := s.sessionID()
	if err != nil {
		return 0, err
	}
	if executionFailed(ctx) {
		return 0, fmt.Errorf("previous task persistence failed")
	}
	request := &apiv1alpha1.TaskStoreServiceCreateTaskRequest{SessionId: id, Task: wire}
	if state, ok := ctx.Value(executionKey{}).(*execution); ok {
		request.DispatchId = state.dispatchID
	}
	var response *apiv1alpha1.TaskStoreServiceCreateTaskResponse
	err = s.retry(ctx, func(ctx context.Context) error {
		var err error
		response, err = s.client.TaskStoreService().CreateTask(ctx, request)
		return err
	})
	if err != nil {
		recordSaveFailure(ctx)
		return 0, sdkError(err)
	}
	recordSave(ctx, task, response.Version)
	return sdktaskstore.TaskVersion(response.Version), nil
}

func (s *Store) Update(ctx context.Context, update *sdktaskstore.UpdateRequest) (sdktaskstore.TaskVersion, error) {
	if executionFailed(ctx) {
		return 0, fmt.Errorf("previous task persistence failed")
	}
	task, err := pbconv.ToProtoTask(update.Task)
	if err != nil {
		return 0, err
	}
	event, err := pbconv.ToProtoStreamResponse(update.Event)
	if err != nil {
		return 0, err
	}
	id, err := s.sessionID()
	if err != nil {
		return 0, err
	}
	request := &apiv1alpha1.TaskStoreServiceUpdateTaskRequest{SessionId: id, Task: task, Event: event, ExpectedVersion: int64(update.PrevVersion)}
	if state, ok := ctx.Value(executionKey{}).(*execution); ok {
		request.DispatchId = state.dispatchID
	}
	var response *apiv1alpha1.TaskStoreServiceUpdateTaskResponse
	err = s.retry(ctx, func(ctx context.Context) error {
		var err error
		response, err = s.client.TaskStoreService().UpdateTask(ctx, request)
		return err
	})
	if err != nil {
		recordSaveFailure(ctx)
		return 0, sdkError(err)
	}
	recordSave(ctx, update.Task, response.Version)
	return sdktaskstore.TaskVersion(response.Version), nil
}

func (s *Store) Get(ctx context.Context, taskID a2a.TaskID) (*sdktaskstore.StoredTask, error) {
	id, err := s.sessionID()
	if err != nil {
		return nil, err
	}
	var response *apiv1alpha1.TaskStoreServiceGetTaskResponse
	err = s.retry(ctx, func(ctx context.Context) error {
		var err error
		response, err = s.client.TaskStoreService().GetTask(ctx, &apiv1alpha1.TaskStoreServiceGetTaskRequest{SessionId: id, TaskId: string(taskID)})
		return err
	})
	if err != nil {
		return nil, sdkError(err)
	}
	current, err := fromStored(response.Stored)
	if err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) List(ctx context.Context, request *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	wire, err := pbconv.ToProtoListTasksRequest(request)
	if err != nil {
		return nil, err
	}
	id, err := s.sessionID()
	if err != nil {
		return nil, err
	}
	var response *apiv1alpha1.TaskStoreServiceListTasksResponse
	err = s.retry(ctx, func(ctx context.Context) error {
		var err error
		response, err = s.client.TaskStoreService().ListTasks(ctx, &apiv1alpha1.TaskStoreServiceListTasksRequest{SessionId: id, Request: wire})
		return err
	})
	if err != nil {
		return nil, sdkError(err)
	}
	return pbconv.FromProtoListTasksResponse(response.Result)
}

// retry retains the same immutable mutation after a lost response. It never
// retries conflicts, advances a version locally, or invokes the agent again.
func (s *Store) retry(ctx context.Context, call func(context.Context) error) error {
	for attempt := range 4 {
		rpcCtx, cancel, err := s.callContext(ctx)
		if err != nil {
			return err
		}
		err = call(rpcCtx)
		cancel()
		if (status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded) || attempt == 3 {
			return err
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func fromStored(stored *apiv1alpha1.StoredTask) (*sdktaskstore.StoredTask, error) {
	if stored == nil || stored.Task == nil || stored.Version <= 0 {
		return nil, fmt.Errorf("TaskStore returned an invalid stored task")
	}
	task, err := pbconv.FromProtoTask(stored.Task)
	if err != nil {
		return nil, err
	}
	return &sdktaskstore.StoredTask{Task: task, Version: sdktaskstore.TaskVersion(stored.Version)}, nil
}

func sdkError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("load runtime task: %w", a2a.ErrTaskNotFound)
	case codes.AlreadyExists:
		return fmt.Errorf("create runtime task: %w", sdktaskstore.ErrTaskAlreadyExists)
	case codes.Aborted:
		return fmt.Errorf("save runtime task: %w", sdktaskstore.ErrConcurrentModification)
	case codes.PermissionDenied, codes.Unauthenticated:
		return a2a.ErrUnauthorized
	case codes.InvalidArgument, codes.FailedPrecondition:
		return a2a.NewError(a2a.ErrInvalidParams, status.Convert(err).Message())
	default:
		return err
	}
}

// GetWorkspace returns the repository checkout this runtime's Session asked for,
// or nil when it asked for none.
func (s *Store) GetWorkspace(ctx context.Context) (*apiv1alpha1.Workspace, error) {
	id, err := s.sessionID()
	if err != nil {
		return nil, err
	}
	var response *apiv1alpha1.TaskStoreServiceGetWorkspaceResponse
	err = s.retry(ctx, func(ctx context.Context) error {
		var err error
		response, err = s.client.TaskStoreService().GetWorkspace(ctx, &apiv1alpha1.TaskStoreServiceGetWorkspaceRequest{SessionId: id})
		return err
	})
	if err != nil {
		return nil, err
	}
	return response.GetWorkspace(), nil
}
