package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retryTestActors struct {
	*lifecycleTestActors
	beforeRead  func(context.Context)
	afterRead   func(context.Context)
	readErr     error
	mutationErr error
	mutations   atomic.Int32
}

// Count even rejected creation requests; AlreadyExists must not mask reissuance.
type issuanceRaceActors struct {
	*lifecycleTestActors
	creations atomic.Int32
	lostReply bool
}

func (a *issuanceRaceActors) create(ctx context.Context, space, name string, create func() (*ateapipb.Actor, error)) (*ateapipb.Actor, error) {
	call := a.creations.Add(1)
	if _, err := a.GetActor(ctx, space, name); status.Code(err) != codes.NotFound {
		return nil, status.Error(codes.AlreadyExists, "Actor name already exists")
	}
	actor, err := create()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	actor.Metadata.Uid = fmt.Sprintf("issuance-%d", call)
	a.actors[actorKey(space, name)].Metadata.Uid = actor.Metadata.Uid
	a.mu.Unlock()
	if call == 1 && a.lostReply {
		return nil, context.DeadlineExceeded
	}
	return actor, nil
}

func (a *issuanceRaceActors) CreateActor(ctx context.Context, space, name, templateSpace, templateName string) (*ateapipb.Actor, error) {
	return a.create(ctx, space, name, func() (*ateapipb.Actor, error) {
		return a.lifecycleTestActors.CreateActor(ctx, space, name, templateSpace, templateName)
	})
}

func (a *issuanceRaceActors) CreateActorFromTag(ctx context.Context, space, name, templateSpace, templateName, tagSpace, tagName string) (*ateapipb.Actor, error) {
	return a.create(ctx, space, name, func() (*ateapipb.Actor, error) {
		return a.lifecycleTestActors.CreateActorFromTag(ctx, space, name, templateSpace, templateName, tagSpace, tagName)
	})
}

type stalePreparerActors struct {
	actorClient
	once     sync.Once
	prepared chan error
	release  <-chan struct{}
}

func (a *stalePreparerActors) GetActor(ctx context.Context, space, name string) (*ateapipb.Actor, error) {
	actor, err := a.actorClient.GetActor(ctx, space, name)
	a.once.Do(func() {
		a.prepared <- err
		select {
		case <-a.release:
		case <-ctx.Done():
		}
	})
	return actor, err
}

type issuanceClaimStore struct {
	*lifecycleTestStore
	claimed   bool
	operation uuid.UUID
}

func (s *issuanceClaimStore) ClaimSessionOperation(ctx context.Context, id string, operation, executor uuid.UUID) (bool, error) {
	claimed, err := s.Client.ClaimSessionOperation(ctx, id, operation, executor)
	s.claimed, s.operation = claimed, operation
	return claimed, err
}

func TestStalePreparerCannotReissueActor(t *testing.T) {
	for _, kind := range []string{"create", "DATA fork"} {
		for _, outcome := range []string{"creation reply lost", "binding commit failed"} {
			for _, absent := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/first Actor absent=%v", kind, outcome, absent), func(t *testing.T) {
					store, session := lifecycleFixture(t)
					base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
					if kind == "DATA fork" {
						session, _ = lifecycleForkFixture(t, store, base, session)
					}
					policyCalls := base.policyCalls
					actors := &issuanceRaceActors{lifecycleTestActors: base, lostReply: outcome == "creation reply lost"}
					prepared, release := make(chan error, 1), make(chan struct{})
					releaseB := sync.OnceFunc(func() { close(release) })
					defer releaseB()
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					bStore := &issuanceClaimStore{lifecycleTestStore: store}
					bActors := &stalePreparerActors{actorClient: actors, prepared: prepared, release: release}
					done := make(chan error, 1)
					go func() {
						_, err := NewActorWorkflow(bStore, bActors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
						done <- err
					}()
					select {
					case err := <-prepared:
						require.Equal(t, codes.NotFound, status.Code(err), "B already prepared the missing Actor before claiming")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					operation, err := store.BeginSessionOperation(ctx, session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE)
					require.NoError(t, err)
					var aStore workflowStore = store
					if outcome == "binding commit failed" {
						aStore = &bindingReplyStore{lifecycleTestStore: store}
					}
					_, err = NewActorWorkflow(aStore, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
					require.Error(t, err)
					before, err := store.GetRuntimeGeneration(ctx, session.Id)
					require.NoError(t, err)
					require.Equal(t, "actor-issued", before.Phase)
					require.Empty(t, before.ActorUID)
					retained, err := store.GetSessionOperation(ctx, session.Id, operation.ID)
					require.NoError(t, err)
					require.Equal(t, operation.ID, retained.ID, "A releases its claim without superseding B's operation")
					if absent {
						require.NoError(t, base.DeleteActor(ctx, before.Atespace, before.ActorName))
					}
					releaseB()
					select {
					case err = <-done:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					after, readErr := store.GetRuntimeGeneration(ctx, session.Id)
					require.NoError(t, readErr)
					t.Logf("B prepared NotFound; A retained operation %s; B claimed=%v; creation calls=%d; target policy calls=%d; phase=%s UID=%q", operation.ID, bStore.claimed, actors.creations.Load(), base.policyCalls-policyCalls, after.Phase, after.ActorUID)
					assert.True(t, bStore.claimed, "B must reach the issue boundary under the same reclaimed operation")
					assert.Equal(t, operation.ID, bStore.operation)
					assert.Error(t, err, "stale preparation cannot grant issuance permission")
					assert.EqualValues(t, 1, actors.creations.Load(), "external creation must be issued exactly once")
					assert.Equal(t, policyCalls, base.policyCalls, "unknown issuance cannot activate policy")
					assert.Equal(t, before, after, "actor-issued identity must stay held, even if its Actor disappears")
				})
			}
		}
	}
}

type issuanceReplyStore struct {
	*lifecycleTestStore
	committed bool
}

func (s *issuanceReplyStore) AdvanceRuntimeGeneration(ctx context.Context, id uuid.UUID, from, to, uid string) error {
	if to == "actor-issued" {
		if s.committed {
			if err := s.Client.AdvanceRuntimeGeneration(ctx, id, from, to, uid); err != nil {
				return err
			}
		}
		return context.DeadlineExceeded
	}
	return s.Client.AdvanceRuntimeGeneration(ctx, id, from, to, uid)
}

func TestUncertainIssuanceWriteDoesNotCreateActor(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			store, session := lifecycleFixture(t)
			writes := &issuanceReplyStore{lifecycleTestStore: store, committed: committed}
			actors := &issuanceRaceActors{lifecycleTestActors: &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}}
			_, err := NewActorWorkflow(writes, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Zero(t, actors.creations.Load(), "an uncertain issuance write never authorizes an external call")
			require.Zero(t, actors.policyCalls)
			generation, err := store.GetRuntimeGeneration(t.Context(), session.Id)
			require.NoError(t, err)
			phase := "secret-issued"
			if committed {
				phase = "actor-issued"
				_, err = NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
				require.Error(t, err, "a lost committed reply leaves permanent issuance hold")
				require.Zero(t, actors.creations.Load())
			}
			require.Equal(t, phase, generation.Phase)
			require.Empty(t, generation.ActorUID)
		})
	}
}

func (a *retryTestActors) GetActor(ctx context.Context, space, name string) (*ateapipb.Actor, error) {
	if a.beforeRead != nil {
		a.beforeRead(ctx)
	}
	if a.readErr != nil {
		return nil, a.readErr
	}
	actor, err := a.lifecycleTestActors.GetActor(ctx, space, name)
	if a.afterRead != nil {
		a.afterRead(ctx)
	}
	return actor, err
}

func (a *retryTestActors) CreateActor(ctx context.Context, space, name, templateSpace, templateName string) (*ateapipb.Actor, error) {
	a.mutations.Add(1)
	actor, err := a.lifecycleTestActors.CreateActor(ctx, space, name, templateSpace, templateName)
	if a.mutationErr != nil {
		return nil, a.mutationErr
	}
	return actor, err
}

func (a *retryTestActors) CreateActorFromTag(ctx context.Context, space, name, templateSpace, templateName, tagSpace, tagName string) (*ateapipb.Actor, error) {
	a.mutations.Add(1)
	actor, err := a.lifecycleTestActors.CreateActorFromTag(ctx, space, name, templateSpace, templateName, tagSpace, tagName)
	if a.mutationErr != nil {
		return nil, a.mutationErr
	}
	return actor, err
}

func (a *retryTestActors) ResumeActor(ctx context.Context, space, name string) (*ateapipb.Actor, error) {
	a.mutations.Add(1)
	actor, err := a.lifecycleTestActors.ResumeActor(ctx, space, name)
	if a.mutationErr != nil {
		return nil, a.mutationErr
	}
	return actor, err
}

func (a *retryTestActors) SuspendActor(ctx context.Context, space, name string) (*ateapipb.Actor, error) {
	a.mutations.Add(1)
	actor, err := a.lifecycleTestActors.SuspendActor(ctx, space, name)
	if a.mutationErr != nil {
		return nil, a.mutationErr
	}
	return actor, err
}

func (a *retryTestActors) DeleteActor(ctx context.Context, space, name string) error {
	a.mutations.Add(1)
	if err := a.lifecycleTestActors.DeleteActor(ctx, space, name); err != nil {
		return err
	}
	return a.mutationErr
}

func TestLifecycleClientRetriesAmbiguousMutation(t *testing.T) {
	for _, name := range []string{"create", "fork", "resume", "suspend", "delete"} {
		t.Run(name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			setup := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			var err error
			if name != "create" && name != "fork" {
				session, err = setup.Create(t.Context(), session)
				require.NoError(t, err)
				switch name {
				case "resume":
					session, err = setup.Suspend(t.Context(), session)
					require.NoError(t, err)
				case "suspend":
					_, err = base.ResumeActor(t.Context(), "team-a", fixtureActorName(t, store, session.Id))
					require.NoError(t, err)
				}
			}
			var checkpointID string
			if name == "fork" {
				session, checkpointID = lifecycleForkFixture(t, store, base, session)
			}
			actors := &retryTestActors{lifecycleTestActors: base, mutationErr: status.Error(codes.Unavailable, "response lost after effect")}
			workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			call := workflow.Create
			switch name {
			case "resume":
				call = workflow.Resume
			case "suspend":
				call = workflow.Suspend
			case "delete":
				call = workflow.Delete
			}
			_, err = call(t.Context(), session)
			if name == "create" || name == "fork" {
				require.Error(t, err)
			} else {
				require.Equal(t, codes.Unavailable, status.Code(err))
			}
			current, err := store.GetSessionByID(t.Context(), session.Id)
			require.NoError(t, err, "uncertainty must preserve the session and its resource pins")
			require.NotEqual(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.Operation)
			if name != "delete" {
				_, err = setup.Delete(t.Context(), current)
				require.ErrorIs(t, err, database.ErrConflict, "Delete must not erase uncertain work")
			}
			if checkpointID != "" {
				_, _, err = store.BeginDeleteSessionCheckpoint(t.Context(), checkpointID, session.Creator)
				require.ErrorIs(t, err, database.ErrNotFound, "uncertain fork must retain its checkpoint pin")
				checkpoint, err := store.GetSessionCheckpoint(t.Context(), checkpointID, session.Creator)
				require.NoError(t, err)
				require.Equal(t, apiv1alpha1.CheckpointState_CHECKPOINT_STATE_READY, checkpoint.State)
			}
			actors.mutationErr = nil
			// A fresh workflow models the next request reaching another replica.
			restarted := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			retryCall := restarted.Create
			switch name {
			case "resume":
				retryCall = restarted.Resume
			case "suspend":
				retryCall = restarted.Suspend
			case "delete":
				retryCall = restarted.Delete
			}
			result, err := retryCall(t.Context(), current)
			if name == "create" || name == "fork" {
				require.Error(t, err)
				generation, readErr := store.GetRuntimeGeneration(t.Context(), current.Id)
				require.NoError(t, readErr)
				require.Equal(t, "actor-issued", generation.Phase)
				require.EqualValues(t, 1, actors.mutations.Load(), "unknown creation must never be adopted or reissued")
				return
			}
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, result.Operation)
			if name == "create" || name == "fork" {
				require.EqualValues(t, 1, actors.mutations.Load(), "retry must preserve the created Actor")
			}
			if name == "delete" {
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, result.State)
			}
		})
	}
}

func TestSupersededLifecycleObserverCannotExecute(t *testing.T) {
	for _, test := range []struct {
		name        string
		deleteAfter bool
		beforeRead  bool
	}{
		{name: "after suspend"},
		{name: "after delete", deleteAfter: true},
		{name: "lookup after delete", deleteAfter: true, beforeRead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			workflow := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			session, err := workflow.Create(t.Context(), session)
			require.NoError(t, err)
			session, err = workflow.Suspend(t.Context(), session)
			require.NoError(t, err)
			read, release := make(chan struct{}), make(chan struct{})
			actors := &retryTestActors{lifecycleTestActors: base, afterRead: func(ctx context.Context) {
				close(read)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}}
			if test.beforeRead {
				actors.beforeRead, actors.afterRead = actors.afterRead, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type outcome struct {
				session *apiv1alpha1.Session
				err     error
			}
			result := make(chan outcome, 1)
			go func() {
				session, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Resume(ctx, session)
				result <- outcome{session, err}
			}()
			select {
			case <-read:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			ready, err := workflow.Resume(ctx, session)
			require.NoError(t, err, "another replica can finish still-unissued work")
			suspended, err := workflow.Suspend(ctx, ready)
			require.NoError(t, err)
			if test.deleteAfter {
				_, err = workflow.Delete(ctx, suspended)
				require.NoError(t, err)
			}
			close(release)
			late := <-result
			require.ErrorIs(t, late.err, database.ErrConflict)
			require.Nil(t, late.session)
			require.Zero(t, actors.mutations.Load(), "the delayed caller must not become another executor")
			current, err := store.GetSessionByID(ctx, session.Id)
			if test.deleteAfter {
				require.ErrorIs(t, err, database.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, current.State)
			}
		})
	}
}

func TestDelayedCreationCannotResurrectDeletedActor(t *testing.T) {
	store, session := lifecycleFixture(t)
	base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	read, release := make(chan struct{}), make(chan struct{})
	actors := &retryTestActors{lifecycleTestActors: base, afterRead: func(ctx context.Context) {
		close(read)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
		result <- err
	}()
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Delete(ctx, session)
	require.NoError(t, err)
	close(release)
	require.ErrorIs(t, <-result, database.ErrConflict)
	require.Zero(t, actors.mutations.Load())
	_, err = base.GetActor(ctx, "team-a", fixtureActorName(t, store, session.Id))
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestLifecycleReadFailureCanRetryPreparation(t *testing.T) {
	store, session := lifecycleFixture(t)
	base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	actors := &retryTestActors{lifecycleTestActors: base, readErr: status.Error(codes.Unavailable, "lookup unavailable")}
	_, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Zero(t, actors.mutations.Load())
	ready, err := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, ready.State)
}

func TestDeletePreparationFailureKeepsAdmissionClosed(t *testing.T) {
	for _, state := range []apiv1alpha1.RuntimeState{
		apiv1alpha1.RuntimeState_RUNTIME_STATE_READY,
		apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			workflow := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			var err error
			if state != apiv1alpha1.RuntimeState_RUNTIME_STATE_CREATING {
				session, err = workflow.Create(t.Context(), session)
				require.NoError(t, err)
			}
			if state == apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED {
				session, err = workflow.Suspend(t.Context(), session)
				require.NoError(t, err)
			}
			actors := &retryTestActors{lifecycleTestActors: base, readErr: status.Error(codes.Unavailable, "lookup unavailable")}
			_, err = NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Delete(t.Context(), session)
			require.Equal(t, codes.Unavailable, status.Code(err))
			require.Zero(t, actors.mutations.Load())
			current, err := store.GetSessionByID(t.Context(), session.Id)
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING, current.State)
			require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, current.Operation)
			require.ErrorIs(t, store.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "next turn"), database.ErrConflict)
			_, err = workflow.Resume(t.Context(), session)
			require.ErrorIs(t, err, database.ErrConflict)
			worker, err := NewExpirationWorker(store, workflow, time.Hour, time.Minute)
			require.NoError(t, err)
			require.ErrorIs(t, worker.expire(t.Context(), session.Id, time.Now()), database.ErrConflict,
				"explicit deletion still requires a client retry")
			deleted, err := workflow.Delete(t.Context(), session)
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED, deleted.State)
			require.Empty(t, base.actors)
		})
	}
}

// completionTestStore injects failures at the database/runtime boundary while
// retaining real PostgreSQL session ownership.
type completionTestStore struct {
	*lifecycleTestStore
	afterClaim func(context.Context)
	finishErr  error
}

func (s *completionTestStore) ClaimSessionOperation(ctx context.Context, sessionID string, id, executor uuid.UUID) (bool, error) {
	claimed, err := s.Client.ClaimSessionOperation(ctx, sessionID, id, executor)
	if claimed && err == nil && s.afterClaim != nil {
		s.afterClaim(ctx)
	}
	return claimed, err
}

func (s *completionTestStore) FinishSessionOperation(ctx context.Context, sessionID string, id, executor uuid.UUID, authority, actorUID, failure string) (*apiv1alpha1.Session, error) {
	if s.finishErr != nil {
		return nil, s.finishErr
	}
	return s.Client.FinishSessionOperation(ctx, sessionID, id, executor, authority, actorUID, failure)
}

func TestLifecycleCompletionFailureRetriesPersistence(t *testing.T) {
	store, session := lifecycleFixture(t)
	actors := &retryTestActors{lifecycleTestActors: &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}}
	failure := status.Error(codes.Unavailable, "completion database unavailable")
	_, err := NewActorWorkflow(&completionTestStore{lifecycleTestStore: store, finishErr: failure}, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
	require.ErrorIs(t, err, failure)
	ready, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, ready.State)
	require.Equal(t, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE, ready.Operation)
	require.EqualValues(t, 1, actors.mutations.Load(), "retry must not recreate the Actor")
}

func TestClaimedCreationBlocksDeletionBeforeRuntimeCall(t *testing.T) {
	store, session := lifecycleFixture(t)
	actors := &retryTestActors{lifecycleTestActors: &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}}
	claimed, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	delayed := &completionTestStore{lifecycleTestStore: store, afterClaim: func(ctx context.Context) {
		close(claimed)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	result := make(chan error, 1)
	go func() {
		_, err := NewActorWorkflow(delayed, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
		result <- err
	}()
	select {
	case <-claimed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
	_, err := workflow.Create(ctx, session)
	require.ErrorIs(t, err, database.ErrConflict)
	_, err = workflow.Delete(ctx, session)
	require.ErrorIs(t, err, database.ErrConflict)
	require.Zero(t, actors.mutations.Load())
	close(release)
	require.NoError(t, <-result)
	require.EqualValues(t, 1, actors.mutations.Load())
}

// Namespace provisioning belongs to the template controller. Lifecycle execution
// must work even when its caller cannot create an Atespace.
func TestExpiredLifecycleAttemptCannotIssueRuntime(t *testing.T) {
	store, session := lifecycleFixture(t)
	actors := &retryTestActors{lifecycleTestActors: &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}}
	claimed, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	attemptCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	delayed := &completionTestStore{lifecycleTestStore: store, afterClaim: func(context.Context) {
		close(claimed)
		<-release // Simulate a stalled executor that does not observe cancellation yet.
	}}
	done := make(chan error, 1)
	go func() {
		_, err := NewActorWorkflow(delayed, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(attemptCtx, session)
		done <- err
	}()
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not claim execution")
	}
	workflow := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
	require.Eventually(t, func() bool {
		_, err := workflow.Create(t.Context(), session)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	// The delayed attempt must observe its expired context before issuing calls.
	// Cleanup releases it after the replacement has finished.
	t.Cleanup(func() {
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		require.EqualValues(t, 1, actors.mutations.Load())
	})
}

func (*retryTestActors) EnsureAtespace(context.Context, string) error {
	return status.Error(codes.PermissionDenied, "session caller cannot create an Atespace")
}

func TestCreationUsesPreparedAtespace(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "fork"}[fork], func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			if fork {
				session, _ = lifecycleForkFixture(t, store, base, session)
			}
			actors := &retryTestActors{lifecycleTestActors: base}
			ready, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(t.Context(), session)
			require.NoError(t, err)
			require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, ready.State)
			require.EqualValues(t, 1, actors.mutations.Load())
		})
	}
}

func TestDelayedCreationObservesOnlyCurrentGeneration(t *testing.T) {
	for _, test := range []struct {
		name      string
		readErr   error
		supersede bool
	}{
		{name: "actor already created", supersede: true},
		{name: "lookup unavailable", readErr: status.Error(codes.Unavailable, "lookup unavailable"), supersede: true},
		{name: "same generation completed"},
		{name: "same generation completed despite local read error", readErr: status.Error(codes.Unavailable, "lookup unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, session := lifecycleFixture(t)
			base := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
			entered, release := make(chan struct{}), make(chan struct{})
			actors := &retryTestActors{lifecycleTestActors: base, readErr: test.readErr, beforeRead: func(ctx context.Context) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type outcome struct {
				session *apiv1alpha1.Session
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
				done <- outcome{result, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			workflow := NewActorWorkflow(store, base, fixtureCredentials{}, "http://kagent-controller.kagent:8083")
			ready, err := workflow.Create(ctx, session)
			require.NoError(t, err)
			if test.supersede {
				_, err = workflow.Suspend(ctx, ready)
				require.NoError(t, err)
			}
			close(release)
			late := <-done
			if test.supersede {
				require.ErrorIs(t, late.err, database.ErrConflict)
				require.Nil(t, late.session)
			} else {
				require.NoError(t, late.err)
				require.Equal(t, ready.Id, late.session.Id)
				require.Equal(t, ready.State, late.session.State)
			}
			require.Zero(t, actors.mutations.Load())
		})
	}
}
