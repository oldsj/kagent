package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func expirationFixture(t *testing.T) (*Client, *apiv1alpha1.Session) {
	t.Helper()
	client := NewClient(setupTestDB(t))
	sessionFixture(t, client, t.Context(), "team-a", "revision-1", "assistant", "kagent")
	session, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "conversation")
	require.NoError(t, err)
	session, err = markSessionReady(t.Context(), client, session.Id, "agent.example")
	require.NoError(t, err)
	return client, session
}

func TestSessionExpirationTaskAdmission(t *testing.T) {
	for _, test := range []struct {
		state a2a.TaskState
		allow bool
	}{
		{a2a.TaskStateSubmitted, false},
		{a2a.TaskStateWorking, false},
		{a2a.TaskStateInputRequired, false},
		{a2a.TaskStateAuthRequired, false},
		{a2a.TaskStateCompleted, true},
		{a2a.TaskStateCanceled, true},
		{a2a.TaskStateFailed, true},
		{a2a.TaskStateRejected, true},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			client, session := expirationFixture(t)
			task := newSessionTask(uuid.NewString(), "message")
			task.ContextID, task.Status.State = session.ContextId, test.state
			snapshot := &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", ContentScope: "FULL"}
			require.NoError(t, saveRuntimeTask(t, client, session.Id, task, task, snapshot))
			_, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, time.Now(), time.Nanosecond)
			if test.allow {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
		})
	}
}

func TestSessionExpirationRechecksActivity(t *testing.T) {
	client, session := expirationFixture(t)
	ctx := t.Context()
	before := time.Now()
	ids, err := client.ListIdleSessions(ctx, before, time.Nanosecond, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{session.Id}, ids)

	task := newSessionTask(uuid.NewString(), "message")
	task.ContextID, task.Status.State = session.ContextId, a2a.TaskStateCompleted
	version, err := client.CreateRuntimeTask(ctx, session.Id, taskMutationHash("completed"), task, "")
	require.NoError(t, err)
	savedBefore := time.Now()
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "unpublished runtime cleanup blocks expiration")
	require.NoError(t, client.SettleSessionTask(ctx, session.Id, string(task.ID), version))
	work, err := client.ClaimSessionQuiescence(ctx)
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrFailedPrecondition, "a claimed suspension blocks expiration")
	require.NoError(t, client.FinishSessionQuiescence(ctx, work, &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", ContentScope: "FULL"}))
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, before, time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "an event after the scan starts a new idle period")
	ids, err = client.ListIdleSessions(ctx, before, time.Nanosecond, "", 100)
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, client.SettleSessionTask(ctx, session.Id, string(task.ID), version))
	deletion, err := client.BeginIdleSessionDeletion(ctx, session.Id, savedBefore, time.Nanosecond)
	require.NoError(t, err, "settlement and its retries must not extend the event's idle clock")
	require.True(t, deletion.IdleSince.After(session.CreatedAt.AsTime()))
	require.False(t, deletion.IdleSince.After(savedBefore))
}

func TestSessionExpirationFencesDispatchAndRetries(t *testing.T) {
	client, session := expirationFixture(t)
	ctx := t.Context()
	dispatch := uuid.New()
	require.NoError(t, client.ReserveSessionDispatch(ctx, session.Id, dispatch, "message"))
	_, err := client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrFailedPrecondition)
	_, err = client.RevokeSessionDispatch(ctx, session.Id, dispatch, "message")
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Time{}, time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "failed lifecycle admission must not persist idle deletion intent")

	work, err := client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING, work.Operation.Instance.State)
	require.ErrorIs(t, client.ReserveSessionDispatch(ctx, session.Id, uuid.New(), "next"), ErrConflict)
	pending, created, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "conversation")
	require.NoError(t, err)
	require.Equal(t, session.Id, pending.Id)
	require.False(t, created, "runtime deletion must finish before the receipt is removed")

	op, err := client.BeginSessionOperation(ctx, session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE)
	require.NoError(t, err)
	require.Equal(t, work.Operation.ID, op.ID, "explicit deletion joins the admitted idle deletion")
	_, err = client.FinishSessionOperation(ctx, session.Id, op.ID, uuid.Nil, "", "", "temporary preparation failure")
	require.NoError(t, err)
	require.ErrorIs(t, client.ReserveSessionDispatch(ctx, session.Id, uuid.New(), "next"), ErrConflict)
	ids, err := client.ListIdleSessions(ctx, time.Time{}, time.Nanosecond, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{session.Id}, ids, "an admitted expiration survives TTL changes")
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Time{}, time.Nanosecond)
	require.NoError(t, err)
	require.NoError(t, deleteSession(ctx, client, session.Id))
	_, err = client.GetSessionByID(ctx, session.Id)
	require.ErrorIs(t, err, ErrNotFound)
	fresh, created, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "conversation")
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, session.Id, fresh.Id)
}

func TestSessionExpirationSkipsLifecycleAndExplicitTombstones(t *testing.T) {
	client, session := expirationFixture(t)
	ctx := t.Context()
	_, err := client.BeginSessionOperation(ctx, session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict)
	_, err = finishSessionOperation(ctx, client, session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, "")
	require.NoError(t, err)
	require.NoError(t, deleteSession(ctx, client, session.Id))
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict)
	ids, err := client.ListIdleSessions(ctx, time.Now(), time.Nanosecond, "", 100)
	require.NoError(t, err)
	require.Empty(t, ids)
	_, created, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "conversation")
	require.ErrorIs(t, err, ErrFailedPrecondition)
	require.False(t, created, "manual deletion keeps its request ID reserved")
}

func TestSessionExpirationCheckpointAdmission(t *testing.T) {
	client, session := expirationFixture(t)
	task := newSessionTask(uuid.NewString(), "message")
	task.ContextID, task.Status.State = session.ContextId, a2a.TaskStateCompleted
	require.NoError(t, saveRuntimeTask(t, client, session.Id, task, task, &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", ContentScope: "DATA"}))
	checkpoint, _, err := client.ReserveSessionCheckpoint(t.Context(), &apiv1alpha1.Checkpoint{
		Id: uuid.NewString(), SessionId: session.Id, HeadTaskId: string(task.ID),
	}, session.Creator, "checkpoint")
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.FinalizeSessionCheckpoint(t.Context(), checkpoint.Id, "tag-uid", "s3://snapshot", "")
	require.NoError(t, err)
	beforeFork := time.Now()
	fork, _, err := client.ForkSession(t.Context(), checkpoint.Id, session.Creator, "fork", uuid.NewString())
	require.NoError(t, err)
	fork, err = markSessionReady(t.Context(), client, fork.Id, "fork.example")
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(t.Context(), fork.Id, beforeFork, time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "a fork's copied events must not shorten its idle lifetime")
	deletion, err := client.BeginIdleSessionDeletion(t.Context(), fork.Id, time.Now(), time.Nanosecond)
	require.NoError(t, err)
	require.Equal(t, fork.CreatedAt.AsTime(), deletion.IdleSince)
	_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, time.Now(), time.Nanosecond)
	require.NoError(t, err, "a completed checkpoint must not keep the session alive")
}

func TestSessionExpirationPaginatesCandidates(t *testing.T) {
	client, first := expirationFixture(t)
	for range 2 {
		_, _, err := client.CreateSession(t.Context(), newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), uuid.NewString())
		require.NoError(t, err)
	}
	_, err := client.BeginIdleSessionDeletion(t.Context(), first.Id, time.Now().Add(-time.Hour), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict, "a new session without events receives the full idle lifetime")
	ids, err := client.ListIdleSessions(t.Context(), time.Now(), time.Nanosecond, "", 2)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	next, err := client.ListIdleSessions(t.Context(), time.Now(), time.Nanosecond, ids[1], 2)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.NotContains(t, ids, next[0])
	require.Contains(t, append(ids, next...), first.Id)
}

func TestSessionExpirationIgnoresMetadataAndLifecycle(t *testing.T) {
	client, session := expirationFixture(t)
	before := time.Now()
	_, err := client.UpdateSessionName(t.Context(), session.Id, session.Creator, "renamed")
	require.NoError(t, err)
	_, err = finishSessionOperation(t.Context(), client, session.Id, apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, "")
	require.NoError(t, err)
	_, err = client.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	deletion, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, before, time.Nanosecond)
	require.NoError(t, err)
	require.Equal(t, session.CreatedAt.AsTime(), deletion.IdleSince)
}

func TestSessionExpirationAgentOverrides(t *testing.T) {
	client, session := expirationFixture(t)
	ctx := t.Context()
	definition := AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid", DesiredRevision: "revision-1"}
	zero := int64(0)
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{Seconds: &zero}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	ids, err := client.ListIdleSessions(ctx, time.Now().Add(30*24*time.Hour), time.Hour, "", 100)
	require.NoError(t, err)
	require.Empty(t, ids)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now().Add(30*24*time.Hour), time.Hour)
	require.ErrorIs(t, err, ErrConflict, "zero override protects existing Sessions")

	short := int64(2)
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{Seconds: &short}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	now := session.CreatedAt.AsTime().Add(3 * time.Second)
	ids, err = client.ListIdleSessions(ctx, now, 0, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{session.Id}, ids, "explicit TTL works with controller default disabled")
	// Admission must read the updated TTL even after a candidate was scanned.
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{Seconds: &zero}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 0)
	require.ErrorIs(t, err, ErrConflict)
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{Seconds: &short}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 0)
	require.NoError(t, err)
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{Seconds: &zero}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	ids, err = client.ListIdleSessions(ctx, now, 0, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{session.Id}, ids, "admitted deletion survives disabling TTL")
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 0)
	require.NoError(t, err)
}

func TestSessionExpirationUsesDefaultAfterOverrideRemoved(t *testing.T) {
	client, session := expirationFixture(t)
	ctx := t.Context()
	zero := int64(0)
	definition := AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid", DesiredRevision: "revision-1", SessionIdleTTL: &SessionIdleTTLPolicy{Seconds: &zero}}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	definition.SessionIdleTTL = &SessionIdleTTLPolicy{}
	require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
	now := session.CreatedAt.AsTime().Add(3 * time.Second)
	_, err := client.BeginIdleSessionDeletion(ctx, session.Id, now, time.Hour)
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 0)
	require.ErrorIs(t, err, ErrConflict)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, time.Second)
	require.NoError(t, err)
}

func TestSessionExpirationPreservesUnresolvedOverride(t *testing.T) {
	for _, seconds := range []int64{0, 30 * 24 * 60 * 60} {
		t.Run((time.Duration(seconds) * time.Second).String(), func(t *testing.T) {
			client, session := expirationFixture(t)
			ctx := t.Context()
			definition := AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid", DesiredRevision: "revision-1", SessionIdleTTL: &SessionIdleTTLPolicy{Seconds: &seconds}}
			require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
			definition.SessionIdleTTL = nil
			definition.DesiredRevision = ""
			require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
			now := session.CreatedAt.AsTime().Add(8 * 24 * time.Hour)
			ids, err := client.ListIdleSessions(ctx, now, 7*24*time.Hour, "", 100)
			require.NoError(t, err)
			if seconds == 0 {
				require.Empty(t, ids)
			}
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 7*24*time.Hour)
			require.ErrorIs(t, err, ErrConflict)
			definition.SessionIdleTTL = &SessionIdleTTLPolicy{}
			require.NoError(t, client.UpsertAgentDefinition(ctx, definition))
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, now, 7*24*time.Hour)
			require.NoError(t, err, "resolved omission restores the default")
		})
	}
}
