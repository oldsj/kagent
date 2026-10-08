package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeAssociationPostgres(t *testing.T) {
	// One owned database, two independent pools. Changes on the peer must
	// commit while the Actor observation is held, proving no lifecycle lock or
	// transaction is held across that read. No real Actor or credential effects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	t.Cleanup(cancel)
	conn := dbtest.StartT(ctx, t)
	dbtest.MigrateT(t, conn, false)
	config, err := pgxpool.ParseConfig(conn)
	require.NoError(t, err)
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	peer, err := pgxpool.NewWithConfig(ctx, config.Copy())
	require.NoError(t, err)
	t.Cleanup(peer.Close)
	client, peerClient := database.NewClient(pool), database.NewClient(peer)
	revision := &database.RuntimeRevision{Revision: "association-revision", Namespace: "team-a", AgentName: "assistant", AgentUID: "template-uid",
		SourceSnapshot: []byte("{}"), AgentCard: &a2apb.AgentCard{Name: "assistant"}, EgressDestinations: []string{},
		ActorTemplateAtespace: "team-a", ActorTemplateName: "assistant-kagent-revision", ActorTemplateUID: "actor-template-uid"}
	require.NoError(t, client.UpsertAgentDefinition(ctx, database.AgentDefinition{Namespace: revision.Namespace, AgentName: revision.AgentName, AgentUID: revision.AgentUID, DesiredRevision: revision.Revision}))
	require.NoError(t, client.RecordRuntimeRevision(ctx, *revision, true))
	store := &lifecycleTestStore{Client: client, revision: revision}
	newRuntime := func(t *testing.T) (*api.Session, *lifecycleTestActors, *associationTestActors) {
		t.Helper()
		session, _, err := client.CreateSession(ctx, &api.Session{Id: uuid.NewString(), Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "team-a", Name: "assistant"}}, uuid.NewString())
		require.NoError(t, err)
		actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
		session, err = NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, session)
		require.NoError(t, err)
		generation, err := client.GetRuntimeGeneration(ctx, session.Id)
		require.NoError(t, err)
		actor := actors.actors[actorKey(generation.Atespace, generation.ActorName)]
		actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		return session, actors, &associationTestActors{actor: proto.CloneOf(actor)}
	}
	for _, state := range []string{"missing generation", "inactive generation", "revoked generation", "unbound Session UID", "suspended Actor"} {
		t.Run(state+" before observation", func(t *testing.T) {
			session, _, actors := newRuntime(t)
			switch state {
			case "missing generation":
				_, err := peer.Exec(ctx, `DELETE FROM runtime_generation WHERE session_id = $1`, session.Id)
				require.NoError(t, err)
			case "inactive generation":
				_, err := peer.Exec(ctx, `UPDATE runtime_generation SET phase = 'bound' WHERE session_id = $1`, session.Id)
				require.NoError(t, err)
			case "revoked generation":
				require.NoError(t, peerClient.RevokeRuntimeGeneration(ctx, session.Id))
			case "unbound Session UID":
				_, err := peer.Exec(ctx, `UPDATE session SET actor_uid = 'other' WHERE id = $1`, session.Id)
				require.NoError(t, err)
			case "suspended Actor":
				actors.actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
			}
			service := NewService(client, serviceTestAuthorizer{}, NewActorWorkflow(client, actors, nil, ""))
			result, err := service.Get(serviceTestContext("mainloop"), session.Id)
			require.NoError(t, err)
			require.Nil(t, result.RuntimeAssociation)
			if state != "suspended Actor" {
				require.Zero(t, actors.calls)
			}
		})
	}
	t.Run("current owned mapping and no persistence", func(t *testing.T) {
		session, _, actors := newRuntime(t)
		_, err := peer.Exec(ctx, `UPDATE runtime_generation SET credential_uri = $2, token_digest = $3 WHERE session_id = $1`, session.Id, "ate-secret://sentinel-private-reference/token", []byte(strings.Repeat("sentinel", 4)))
		require.NoError(t, err)
		service := NewService(client, serviceTestAuthorizer{}, NewActorWorkflow(client, actors, nil, ""))
		result, err := service.Get(serviceTestContext("mainloop"), session.Id)
		require.NoError(t, err)
		generation, err := peerClient.GetRuntimeGeneration(ctx, session.Id)
		require.NoError(t, err)
		require.Equal(t, generation.ID.String(), result.GetRuntimeAssociation().GetGenerationId())
		require.Equal(t, generation.ActorUID, result.GetRuntimeAssociation().GetActorUid())
		require.True(t, result.GetRuntimeAssociation().GetCurrentActive())
		require.NotEqual(t, result.Id, result.GetRuntimeAssociation().GetActorUid())
		assertAssociationResponsePrivate(t, result)
		var data []byte
		require.NoError(t, peer.QueryRow(ctx, `SELECT data FROM session WHERE id = $1`, session.Id).Scan(&data))
		stored := &api.Session{}
		require.NoError(t, proto.Unmarshal(data, stored))
		require.Nil(t, stored.RuntimeAssociation)
		page, err := service.List(serviceTestContext("mainloop"), ListRequest{})
		require.NoError(t, err)
		for _, item := range page.Sessions {
			require.Nil(t, item.RuntimeAssociation)
		}
		require.Equal(t, 1, actors.calls, "List must not inspect Actors")
	})
	for _, change := range []string{"revoke", "pending suspend", "delete", "replace UID", "replace generation", "suspended Session"} {
		t.Run(change+" during Actor read", func(t *testing.T) {
			session, _, actors := newRuntime(t)
			entered, release := make(chan struct{}), make(chan struct{})
			releaseActor := sync.OnceFunc(func() { close(release) })
			defer releaseActor()
			actors.hook = func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			service := NewService(client, serviceTestAuthorizer{}, NewActorWorkflow(client, actors, nil, ""))
			type observation struct {
				session *api.Session
				err     error
			}
			result := make(chan observation, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				observed, err := service.Get(authenticatedAssociationContext(ctx), session.Id)
				result <- observation{observed, err}
			}()
			defer func() {
				releaseActor()
				select {
				case <-finished:
				case <-ctx.Done():
				}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("Actor observation did not begin")
			}
			// Every mutation uses a second connection and has to finish before
			// releasing the read. Deletion uses the real admission/finish path.
			mutationCtx, mutationCancel := context.WithTimeout(ctx, 10*time.Second)
			defer mutationCancel()
			switch change {
			case "revoke":
				require.NoError(t, peerClient.RevokeRuntimeGeneration(mutationCtx, session.Id))
			case "pending suspend":
				_, err := peerClient.BeginSessionOperation(mutationCtx, session.Id, api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
				require.NoError(t, err)
			case "delete":
				operation, err := peerClient.BeginSessionOperation(mutationCtx, session.Id, api.RuntimeOperation_RUNTIME_OPERATION_DELETE)
				require.NoError(t, err)
				executor := uuid.New()
				claimed, err := peerClient.ClaimSessionOperation(mutationCtx, session.Id, operation.ID, executor)
				require.NoError(t, err)
				require.True(t, claimed)
				_, err = peerClient.FinishSessionOperation(mutationCtx, session.Id, operation.ID, executor, session.A2AAuthority, "actor-uid", "")
				require.NoError(t, err)
			case "replace UID":
				// Production forbids same-Session replacement. Controlled SQL
				// exercises refusal if durable identity changes regardless.
				_, err := peer.Exec(mutationCtx, `UPDATE runtime_generation SET actor_uid = 'replacement-uid' WHERE session_id = $1`, session.Id)
				require.NoError(t, err)
			case "replace generation":
				_, err := peer.Exec(mutationCtx, `UPDATE runtime_generation SET id = $2 WHERE session_id = $1`, session.Id, uuid.New())
				require.NoError(t, err)
			case "suspended Session":
				_, err := peer.Exec(mutationCtx, `UPDATE runtime_instance SET state = 'RUNTIME_STATE_SUSPENDED' WHERE id = $1`, session.Id)
				require.NoError(t, err)
			}
			// Channel handoff prevents the next subtest starting before the read
			// finishes; deferred close still unblocks it on a failed assertion.
			releaseActor()
			select {
			case observed := <-result:
				require.NoError(t, observed.err)
				require.Nil(t, observed.session.RuntimeAssociation)
			case <-ctx.Done():
				t.Fatal("runtime observation did not finish")
			}
		})
	}
	t.Run("fork never inherits the observed association", func(t *testing.T) {
		session, actors, reader := newRuntime(t)
		service := NewService(client, serviceTestAuthorizer{}, NewActorWorkflow(client, reader, nil, ""))
		observed, err := service.Get(serviceTestContext("mainloop"), session.Id)
		require.NoError(t, err)
		require.NotNil(t, observed.RuntimeAssociation)
		fork, _ := lifecycleForkFixture(t, store, actors, observed)
		require.Nil(t, fork.RuntimeAssociation)
		_, err = client.GetRuntimeGeneration(ctx, fork.Id)
		require.ErrorIs(t, err, database.ErrNotFound)
		fork, err = NewActorWorkflow(store, actors, fixtureCredentials{}, "http://kagent-controller.kagent:8083").Create(ctx, fork)
		require.NoError(t, err)
		require.Nil(t, fork.RuntimeAssociation)
		generation, err := client.GetRuntimeGeneration(ctx, fork.Id)
		require.NoError(t, err)
		require.NotEqual(t, observed.RuntimeAssociation.GenerationId, generation.ID.String())
		require.NotEqual(t, observed.RuntimeAssociation.ActorName, generation.ActorName)
	})
}

func authenticatedAssociationContext(ctx context.Context) context.Context {
	return auth.AuthSessionTo(ctx, mainloopSession{})
}
