package taskstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
)

type callbackStore struct {
	binding      database.RuntimeGeneration
	effects      int
	gates        int
	revoked      bool
	task         *a2a.Task
	quiesceDelay time.Duration
}

func (s *callbackStore) WithRuntimeGeneration(_ context.Context, g database.RuntimeGeneration, fn func(database.RuntimeTaskStore) error) error {
	s.gates++
	if s.revoked || g.ID != s.binding.ID {
		return database.ErrNotFound
	}
	return fn(s)
}
func (s *callbackStore) GetSessionForRuntime(_ context.Context, id, uid string) (*apiv1alpha1.Session, error) {
	s.effects++
	if id != s.binding.SessionID.String() || uid != s.binding.ActorUID {
		return nil, database.ErrNotFound
	}
	return &apiv1alpha1.Session{Id: id, ContextId: id, A2AAuthority: substrate.ActorHost(s.binding.Atespace, s.binding.ActorName, "")}, nil
}
func (s *callbackStore) CreateRuntimeTask(context.Context, string, []byte, *a2a.Task, string) (int64, error) {
	s.effects++
	return 1, nil
}
func (s *callbackStore) UpdateSessionTask(context.Context, string, int64, []byte, *a2a.Task, a2a.Event, string) (int64, error) {
	s.effects++
	return 2, nil
}
func (s *callbackStore) GetVersionedSessionTask(_ context.Context, _ string, id string) (*a2a.Task, int64, error) {
	s.effects++
	if id != "own-task" {
		return nil, 0, database.ErrNotFound
	}
	return s.task, 1, nil
}
func (s *callbackStore) ListSessionTasks(context.Context, string, string, a2a.TaskState, *time.Time, int, *int) ([]*a2a.Task, int, error) {
	s.effects++
	return []*a2a.Task{s.task}, 1, nil
}
func (s *callbackStore) SettleSessionTask(_ context.Context, _ string, id string, _ int64, delay time.Duration) error {
	s.effects++
	s.quiesceDelay = delay
	if id != "own-task" {
		return database.ErrNotFound
	}
	return nil
}

type callbackActors struct {
	binding     database.RuntimeGeneration
	reads       int
	uid         string
	unavailable bool
}

func (s *callbackActors) GetActor(context.Context, string, string) (*ateapipb.Actor, error) {
	s.reads++
	if s.unavailable {
		return nil, fmt.Errorf("unavailable")
	}
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: s.binding.Atespace, Name: s.binding.ActorName, Uid: s.uid}, Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}}, nil
}

func TestEveryCallbackChecksGenerationBeforeData(t *testing.T) {
	for _, method := range []string{"CreateTask", "GetTask", "UpdateTask", "ListTasks", "SettleTask", "GetWorkspace"} {
		for _, failure := range []string{"own", "foreign Session", "stale UID", "lookup unavailable", "revoked"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				id := uuid.New()
				binding := database.RuntimeGeneration{ID: uuid.New(), SessionID: id, Atespace: "team-a", ActorName: "session-" + id.String() + "-0123456789abcdef", ActorUID: "uid-a", Phase: "active"}
				message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("fixture"))
				message.ContextID = id.String()
				task := a2a.NewSubmittedTask(message, message)
				task.ID = "own-task"
				store := &callbackStore{binding: binding, task: task}
				actors := &callbackActors{binding: binding, uid: binding.ActorUID}
				requested := id.String()
				switch failure {
				case "foreign Session":
					requested = uuid.NewString()
				case "stale UID":
					actors.uid = "replacement"
				case "lookup unavailable":
					actors.unavailable = true
				case "revoked":
					store.revoked = true
				}
				ctx := auth.AuthSessionTo(t.Context(), runtimeSession{binding: binding})
				service := NewService(store, actors, 0)
				wire := &a2apb.Task{Id: "own-task", ContextId: id.String(), Status: &a2apb.TaskStatus{State: a2apb.TaskState_TASK_STATE_SUBMITTED}}
				var err error
				switch method {
				case "CreateTask":
					_, err = service.CreateTask(ctx, &apiv1alpha1.TaskStoreServiceCreateTaskRequest{SessionId: requested, Task: wire})
				case "GetTask":
					_, err = service.GetTask(ctx, &apiv1alpha1.TaskStoreServiceGetTaskRequest{SessionId: requested, TaskId: "own-task"})
				case "UpdateTask":
					_, err = service.UpdateTask(ctx, &apiv1alpha1.TaskStoreServiceUpdateTaskRequest{SessionId: requested, Task: wire, ExpectedVersion: 1})
				case "ListTasks":
					_, err = service.ListTasks(ctx, &apiv1alpha1.TaskStoreServiceListTasksRequest{SessionId: requested, Request: &a2apb.ListTasksRequest{}})
				case "SettleTask":
					_, err = service.SettleTask(ctx, &apiv1alpha1.TaskStoreServiceSettleTaskRequest{SessionId: requested, TaskId: "own-task", Version: 1})
				case "GetWorkspace":
					_, err = service.GetWorkspace(ctx, &apiv1alpha1.TaskStoreServiceGetWorkspaceRequest{SessionId: requested})
				}
				if failure == "own" {
					require.NoError(t, err)
					require.Positive(t, store.effects)
					require.Equal(t, 1, actors.reads)
				} else {
					require.Error(t, err)
					require.Zero(t, store.effects)
				}
			})
		}
	}
}

func TestForeignTaskIDsCannotSelectAnotherHistory(t *testing.T) {
	id := uuid.New()
	binding := database.RuntimeGeneration{ID: uuid.New(), SessionID: id, Atespace: "team", ActorName: "issued", ActorUID: "uid", Phase: "active"}
	store := &callbackStore{binding: binding}
	actors := &callbackActors{binding: binding, uid: "uid"}
	service := NewService(store, actors, 0)
	ctx := auth.AuthSessionTo(t.Context(), runtimeSession{binding: binding})
	_, err := service.GetTask(ctx, &apiv1alpha1.TaskStoreServiceGetTaskRequest{SessionId: id.String(), TaskId: "foreign-task"})
	require.Error(t, err)
	_, err = service.SettleTask(ctx, &apiv1alpha1.TaskStoreServiceSettleTaskRequest{SessionId: id.String(), TaskId: "foreign-task", Version: 1})
	require.Error(t, err)
	require.Equal(t, 2, actors.reads, "each RPC performs an uncached observation")
}

func TestSettlementPreservesConfiguredDelayInsideAuthorization(t *testing.T) {
	id := uuid.New()
	binding := database.RuntimeGeneration{ID: uuid.New(), SessionID: id, Atespace: "team", ActorName: "issued", ActorUID: "uid", Phase: "active"}
	store := &callbackStore{binding: binding}
	actors := &callbackActors{binding: binding, uid: "uid"}
	service := NewService(store, actors, 15*time.Minute)
	ctx := auth.AuthSessionTo(t.Context(), runtimeSession{binding: binding})
	_, err := service.SettleTask(ctx, &apiv1alpha1.TaskStoreServiceSettleTaskRequest{SessionId: id.String(), TaskId: "own-task", Version: 1})
	require.NoError(t, err)
	require.Equal(t, 15*time.Minute, store.quiesceDelay)
	require.Equal(t, 1, store.gates)
}

func TestPreparationCallbackCannotUsePublicOrUnconfiguredAuthority(t *testing.T) {
	id := uuid.NewString()
	assignment := &apiv1alpha1.NativeWorkspacePreparation{SessionId: id, ContextId: id, CreateRequestId: "create", ActionId: "create:prepare", RequestDigest: strings.Repeat("a", 64), ExecutionId: uuid.NewString(), ChallengeId: uuid.NewString(), GenerationId: uuid.NewString(), Atespace: "kagent", ActorName: "owned", ActorUid: "owned-uid", PreparedRevision: "original", Workspace: &apiv1alpha1.Workspace{Repo: "https://github.com/owner/repo.git", Ref: strings.Repeat("a", 40), Branch: "feature"}, DevelopmentImage: "fixture/d", Platform: "linux/amd64", PolicyIdentity: "original", PayloadImage: "fixture/r", Provider: "codex", Schema: 1, CliVersion: "1.0", Profile: "child", SetupDigest: strings.Repeat("a", 64), ConfigDigest: strings.Repeat("a", 64), McpDigest: strings.Repeat("a", 64)}
	request := &apiv1alpha1.TaskStoreServiceCompleteWorkspacePreparationRequest{SessionId: id, Assignment: assignment, ObservedAt: timestamppb.Now()}
	for _, name := range []string{"public", "missing", "unconfigured"} {
		t.Run(name, func(t *testing.T) {
			store := &callbackStore{}
			service := NewService(store, nil, 0)
			ctx := t.Context()
			if name == "public" {
				ctx = auth.AuthSessionTo(ctx, auth.ControlPlaneSession{})
			}
			if name == "unconfigured" {
				ctx = auth.AuthSessionTo(ctx, runtimeSession{binding: database.RuntimeGeneration{SessionID: uuid.MustParse(id)}})
			}
			_, err := service.CompleteWorkspacePreparation(ctx, request)
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.Zero(t, store.effects)
		})
	}
	request.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	_, err := NewService(&callbackStore{}, nil, 0).CompleteWorkspacePreparation(t.Context(), request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

type preparationGateStore struct {
	*callbackStore
	required bool
	assigned int
}

func (s *preparationGateStore) WithRuntimeGeneration(_ context.Context, _ database.RuntimeGeneration, fn func(database.RuntimeTaskStore) error) error {
	return fn(s)
}
func (s *preparationGateStore) GetSessionForRuntime(ctx context.Context, id, uid string) (*apiv1alpha1.Session, error) {
	session, err := s.callbackStore.GetSessionForRuntime(ctx, id, uid)
	if err != nil {
		return nil, err
	}
	session.Creator = auth.MainloopService
	session.Workspace = &apiv1alpha1.Workspace{Repo: "https://github.com/owner/repo.git"}
	session.DevelopmentEnvironment = &apiv1alpha1.DevelopmentEnvironment{Image: "fixture"}
	return session, nil
}
func (s *preparationGateStore) WorkspacePreparationRequired(context.Context, *apiv1alpha1.Session) (bool, error) {
	return s.required, nil
}
func (s *preparationGateStore) AssignWorkspacePreparation(context.Context, string) (*apiv1alpha1.NativeWorkspacePreparation, bool, error) {
	s.assigned++
	return nil, true, nil
}

func TestGetWorkspaceRequiresPreparationOnlyWhenStoreAdmissionDoes(t *testing.T) {
	for name, required := range map[string]bool{"direct Git": false, "enforcing proxies": true} {
		t.Run(name, func(t *testing.T) {
			id := uuid.New()
			binding := database.RuntimeGeneration{ID: uuid.New(), SessionID: id, Atespace: "team-a", ActorName: "session-" + id.String() + "-0123456789abcdef", ActorUID: "uid-a", Phase: "active"}
			store := &preparationGateStore{callbackStore: &callbackStore{binding: binding}, required: required}
			service := NewService(store, &callbackActors{binding: binding, uid: binding.ActorUID}, 0)
			ctx := auth.AuthSessionTo(t.Context(), runtimeSession{binding: binding})
			result, err := service.GetWorkspace(ctx, &apiv1alpha1.TaskStoreServiceGetWorkspaceRequest{SessionId: id.String()})
			require.NoError(t, err)
			require.Equal(t, "https://github.com/owner/repo.git", result.GetWorkspace().GetRepo())
			require.Equal(t, required, result.GetPreparationRequired())
			require.Equal(t, required, result.GetPreparationReady())
			if required {
				require.Equal(t, 1, store.assigned)
			} else {
				require.Zero(t, store.assigned, "direct-Git Sessions never issue a preparation")
			}
		})
	}
}
