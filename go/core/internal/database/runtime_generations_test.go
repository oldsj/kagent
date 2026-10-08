package database

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func generationCandidate(id string) RuntimeGeneration {
	generationID := uuid.New()
	digest := sha256.Sum256([]byte(generationID.String()))
	return RuntimeGeneration{ID: generationID, SessionID: uuid.MustParse(id), Atespace: "team-a", ActorName: "session-" + id + "-" + strings.ReplaceAll(generationID.String(), "-", "")[:16], CredentialURI: "ate-secret://k8s.io/default/kagent/kagent-runtime-" + generationID.String() + "/token", TokenDigest: digest[:]}
}
func generationFixture(t *testing.T) (*Client, *apiv1alpha1.Session, *RuntimeGeneration) {
	t.Helper()
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	candidate := generationCandidate(session.Id)
	generation, allocated, err := client.AllocateRuntimeGeneration(t.Context(), candidate)
	require.NoError(t, err)
	require.True(t, allocated)
	return client, session, generation
}
func activateGeneration(t *testing.T, client *Client, g *RuntimeGeneration) {
	t.Helper()
	for _, phase := range []struct{ from, to, uid string }{{"allocated", "secret-issued", ""}, {"secret-issued", "actor-issued", ""}, {"actor-issued", "bound", "actor-uid"}, {"bound", "active", "actor-uid"}} {
		require.NoError(t, client.AdvanceRuntimeGeneration(t.Context(), g.ID, phase.from, phase.to, phase.uid))
	}
	g.Phase, g.ActorUID = "active", "actor-uid"
}

func TestGenerationAllocationRetainsIssuanceAndTombstone(t *testing.T) {
	client, session, g := generationFixture(t)
	retry, allocated, err := client.AllocateRuntimeGeneration(t.Context(), generationCandidate(session.Id))
	require.NoError(t, err)
	require.False(t, allocated)
	require.Equal(t, g.ID, retry.ID)
	require.Equal(t, g.ActorName, retry.ActorName)
	require.Equal(t, g.CredentialURI, retry.CredentialURI)
	require.Equal(t, g.TokenDigest, retry.TokenDigest)
	require.ErrorIs(t, client.AdvanceRuntimeGeneration(t.Context(), g.ID, "allocated", "active", ""), ErrFailedPrecondition)
	activateGeneration(t, client, g)
	require.ErrorIs(t, client.AdvanceRuntimeGeneration(t.Context(), g.ID, "actor-issued", "bound", "foreign-uid"), ErrConflict)
	require.NoError(t, client.RevokeRuntimeGeneration(t.Context(), session.Id))
	_, _, err = client.AllocateRuntimeGeneration(t.Context(), generationCandidate(session.Id))
	require.ErrorIs(t, err, ErrFailedPrecondition)
	retained, err := client.GetRuntimeGeneration(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, "revoked", retained.Phase)
	require.NoError(t, deleteSession(t.Context(), client, session.Id))
	retained, err = client.GetRuntimeGeneration(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, g.ID, retained.ID, "retirement survives Session deletion")
	_, err = client.GetRuntimeGenerationByDigest(t.Context(), g.TokenDigest)
	require.ErrorIs(t, err, ErrNotFound)
	// The unique tombstones apply even when another Session attempts reuse.
	second, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	collision := generationCandidate(second.Id)
	collision.ActorName = g.ActorName
	_, _, err = client.AllocateRuntimeGeneration(t.Context(), collision)
	require.Error(t, err)
	collision = generationCandidate(second.Id)
	collision.CredentialURI = g.CredentialURI
	_, _, err = client.AllocateRuntimeGeneration(t.Context(), collision)
	require.Error(t, err)
}

func TestUnledgeredPreCutoverSessionCannotEnroll(t *testing.T) {
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
	require.NoError(t, err)
	// Deliberate pre-migration durable fixture; ordinary new inserts set true.
	_, err = client.db.Exec(t.Context(), `UPDATE session SET runtime_generation_eligible = false WHERE id = $1`, session.Id)
	require.NoError(t, err)
	_, _, err = client.AllocateRuntimeGeneration(t.Context(), generationCandidate(session.Id))
	require.ErrorIs(t, err, ErrFailedPrecondition)
	_, err = client.GetRuntimeGeneration(t.Context(), session.Id)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestActorIssuanceTransitionHasOneWinner(t *testing.T) {
	client, session, generation := generationFixture(t)
	require.NoError(t, client.AdvanceRuntimeGeneration(t.Context(), generation.ID, "allocated", "secret-issued", ""))
	start, outcomes := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			outcomes <- client.AdvanceRuntimeGeneration(t.Context(), generation.ID, "secret-issued", "actor-issued", "")
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-outcomes; err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, ErrConflict)
		}
	}
	require.Equal(t, 1, winners, "acknowledging actor-issued cannot grant another creation call")
	require.ErrorIs(t, client.AdvanceRuntimeGeneration(t.Context(), generation.ID, "secret-issued", "actor-issued", ""), ErrConflict)
	held, err := client.GetRuntimeGeneration(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, "actor-issued", held.Phase)
	require.Empty(t, held.ActorUID)
	for _, transition := range []struct{ from, to string }{{"actor-issued", "bound"}, {"bound", "active"}} {
		for range 2 {
			require.NoError(t, client.AdvanceRuntimeGeneration(t.Context(), generation.ID, transition.from, transition.to, "original-uid"), "exact-UID completion remains idempotent")
		}
	}
	require.ErrorIs(t, client.AdvanceRuntimeGeneration(t.Context(), generation.ID, "bound", "active", "replacement-uid"), ErrConflict)
}

func TestCallbackTransactionSerializesRevocation(t *testing.T) {
	client, session, g := generationFixture(t)
	activateGeneration(t, client, g)
	entered, release := make(chan struct{}), make(chan struct{})
	callbackDone, revocationDone := make(chan error, 1), make(chan error, 1)
	go func() {
		callbackDone <- client.WithRuntimeGeneration(t.Context(), *g, func(store RuntimeTaskStore) error {
			close(entered)
			<-release
			// The inner read runs inside the same authorized transaction.
			_, _, err := store.GetVersionedSessionTask(t.Context(), session.Id, "absent")
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			return nil
		})
	}()
	<-entered
	go func() { revocationDone <- client.RevokeRuntimeGeneration(t.Context(), session.Id) }()
	close(release)
	require.NoError(t, <-callbackDone)
	require.NoError(t, <-revocationDone)
	effects := 0
	err := client.WithRuntimeGeneration(t.Context(), *g, func(RuntimeTaskStore) error { effects++; return nil })
	require.Error(t, err)
	require.Zero(t, effects, "no callback data access after committed revocation")
}

func TestCallbackTransactionRejectsForgedBinding(t *testing.T) {
	client, _, g := generationFixture(t)
	activateGeneration(t, client, g)
	for _, change := range []func(*RuntimeGeneration){
		func(g *RuntimeGeneration) { g.ActorUID = "foreign" }, func(g *RuntimeGeneration) { g.SessionID = uuid.New() }, func(g *RuntimeGeneration) { g.ActorName = "foreign" }, func(g *RuntimeGeneration) { g.TokenDigest = make([]byte, 32) },
	} {
		forged := *g
		change(&forged)
		effects := 0
		err := client.WithRuntimeGeneration(context.Background(), forged, func(RuntimeTaskStore) error { effects++; return nil })
		require.Error(t, err)
		require.Zero(t, effects)
	}
}
