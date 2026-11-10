package database

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

func expirationFixture(t *testing.T) (*Client, *apiv1.Session) {
	t.Helper()
	client := NewClient(setupTestDB(t), "public")
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
		{a2a.TaskStateInputRequired, true},
		{a2a.TaskStateAuthRequired, true},
		{a2a.TaskStateCompleted, true},
		{a2a.TaskStateCanceled, true},
		{a2a.TaskStateFailed, true},
		{a2a.TaskStateRejected, true},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			client, session := expirationFixture(t)
			task := newSessionTask(uuid.NewString(), "message")
			task.ContextID, task.Status.State = session.ContextId, test.state
			snapshot := &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", Fidelity: "SNAPSHOT_FIDELITY_VOLUMES"}
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

func TestWaitingSessionExpirationBoundary(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			client, session := expirationFixture(t)
			task := newSessionTask(uuid.NewString(), "message")
			task.ContextID, task.Status.State = session.ContextId, state
			require.NoError(t, saveRuntimeTask(t, client, session.Id, task, task, nil))
			stored, err := client.GetSessionTask(t.Context(), session.Id, string(task.ID), nil)
			require.NoError(t, err)
			idleSince, ok := apia2a.TaskCreatedAt(stored)
			require.True(t, ok)

			const ttl = time.Hour
			expiresAt := idleSince.Add(ttl)
			before := expiresAt.Add(-time.Microsecond)
			ids, err := client.ListIdleSessions(t.Context(), before, ttl, "", 100)
			require.NoError(t, err)
			require.Empty(t, ids, "a wait receives the full idle lifetime")
			_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, before, ttl)
			require.ErrorIs(t, err, ErrConflict)

			ids, err = client.ListIdleSessions(t.Context(), expiresAt, ttl, "", 100)
			require.NoError(t, err)
			require.Equal(t, []string{session.Id}, ids)
			deletion, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, expiresAt, ttl)
			require.NoError(t, err, "a wait expires when its idle lifetime is reached")
			require.WithinDuration(t, idleSince, deletion.IdleSince, 0)
		})
	}
}

func TestSessionExpirationRechecksActivity(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			client, session := expirationFixture(t)
			ctx := t.Context()
			before := session.CreatedAt.AsTime()
			ids, err := client.ListIdleSessions(ctx, before, time.Nanosecond, "", 100)
			require.NoError(t, err)
			require.Equal(t, []string{session.Id}, ids)

			task := newSessionTask(uuid.NewString(), "message")
			task.ContextID, task.Status.State = session.ContextId, state
			version, err := client.CreateRuntimeTask(ctx, session.Id, taskMutationHash("idle boundary"), task, "")
			require.NoError(t, err)
			stored, err := client.GetSessionTask(ctx, session.Id, string(task.ID), nil)
			require.NoError(t, err)
			savedBefore, ok := apia2a.TaskCreatedAt(stored)
			require.True(t, ok)
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
			require.ErrorIs(t, err, ErrConflict, "unpublished runtime cleanup blocks expiration")
			require.NoError(t, client.SettleSessionTask(ctx, session.Id, string(task.ID), version))
			work, err := client.ClaimSessionQuiescence(ctx)
			require.NoError(t, err)
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
			require.ErrorIs(t, err, ErrFailedPrecondition, "claimed idle lifecycle work blocks expiration")
			require.NoError(t, client.FinishSessionQuiescence(ctx, work, &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", Fidelity: "SNAPSHOT_FIDELITY_VOLUMES"}))
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, before, time.Nanosecond)
			require.ErrorIs(t, err, ErrConflict, "an event after the scan starts a new idle period")
			ids, err = client.ListIdleSessions(ctx, before, time.Nanosecond, "", 100)
			require.NoError(t, err)
			require.Empty(t, ids)

			require.NoError(t, client.SettleSessionTask(ctx, session.Id, string(task.ID), version))
			deletion, err := client.BeginIdleSessionDeletion(ctx, session.Id, savedBefore.Add(time.Hour), time.Hour)
			require.NoError(t, err, "settlement and its retries must not extend the event's idle clock")
			require.True(t, deletion.IdleSince.After(session.CreatedAt.AsTime()))
			require.False(t, deletion.IdleSince.After(savedBefore))
		})
	}
}

func TestWaitingSessionExpirationFencesContinuation(t *testing.T) {
	for _, state := range []a2a.TaskState{a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			client, session := expirationFixture(t)
			ctx := t.Context()
			task := newSessionTask(uuid.NewString(), "message")
			task.ContextID, task.Status.State = session.ContextId, state
			require.NoError(t, saveRuntimeTask(t, client, session.Id, task, task, nil))
			stored, err := client.GetSessionTask(ctx, session.Id, string(task.ID), nil)
			require.NoError(t, err)
			before, ok := apia2a.TaskCreatedAt(stored)
			require.True(t, ok)
			_, version, err := client.GetVersionedSessionTask(ctx, session.Id, string(task.ID))
			require.NoError(t, err)

			dispatch := uuid.New()
			require.NoError(t, client.ReserveSessionDispatch(ctx, session.Id, dispatch, "reply"))
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
			require.ErrorIs(t, err, ErrFailedPrecondition, "an admitted reply blocks expiration")
			reply := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("continue"))
			reply.ID, reply.TaskID, reply.ContextID = "reply", task.ID, task.ContextID
			version, err = client.UpdateSessionTask(ctx, session.Id, version, taskMutationHash("reply"), task, reply, dispatch.String())
			require.NoError(t, err)
			_, err = client.RevokeSessionDispatch(ctx, session.Id, dispatch, "reply")
			require.NoError(t, err)
			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, before, time.Nanosecond)
			require.ErrorIs(t, err, ErrConflict, "a reply starts a new idle period even while the task remains waiting")
			ids, err := client.ListIdleSessions(ctx, before, time.Nanosecond, "", 100)
			require.NoError(t, err)
			require.Empty(t, ids)

			_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
			require.NoError(t, err)
			require.ErrorIs(t, client.ReserveSessionDispatch(ctx, session.Id, uuid.New(), "next reply"), ErrConflict)
			task.Status.State = a2a.TaskStateWorking
			_, err = client.UpdateSessionTask(ctx, session.Id, version, taskMutationHash("late reply"), task, task, "")
			require.ErrorIs(t, err, ErrConflict, "expiration closes runtime writes before deleting compute")
		})
	}
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
	require.Equal(t, apiv1.RuntimeState_RUNTIME_STATE_DELETING, work.Operation.Instance.State)
	require.ErrorIs(t, client.ReserveSessionDispatch(ctx, session.Id, uuid.New(), "next"), ErrConflict)
	pending, created, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "conversation")
	require.NoError(t, err)
	require.Equal(t, session.Id, pending.Id)
	require.False(t, created, "runtime deletion must finish before the receipt is removed")

	op, err := client.BeginSessionOperation(ctx, session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_DELETE, false)
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
	_, err := client.BeginSessionOperation(ctx, session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, false)
	require.NoError(t, err)
	_, err = client.BeginIdleSessionDeletion(ctx, session.Id, time.Now(), time.Nanosecond)
	require.ErrorIs(t, err, ErrConflict)
	_, err = finishSessionOperation(ctx, client, session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, "")
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
	require.NoError(t, saveRuntimeTask(t, client, session.Id, task, task, &SessionTaskSnapshot{Atespace: "team-a", URI: "s3://snapshot", Fidelity: "SNAPSHOT_FIDELITY_VOLUMES"}))
	checkpoint, _, err := client.ReserveSessionCheckpoint(t.Context(), &apiv1.Checkpoint{
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
	_, err = finishSessionOperation(t.Context(), client, session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, "")
	require.NoError(t, err)
	_, err = client.GetSessionByID(t.Context(), session.Id)
	require.NoError(t, err)
	deletion, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, before, time.Nanosecond)
	require.NoError(t, err)
	require.Equal(t, session.CreatedAt.AsTime(), deletion.IdleSince)
}

func TestSessionExpirationHarnessIdleTTL(t *testing.T) {
	seconds := func(v int64) *SessionIdleTTLPolicy { return &SessionIdleTTLPolicy{Seconds: &v} }
	scenarios := []struct {
		name       string
		policies   []*SessionIdleTTLPolicy // Applied in order; nil means unresolved Harness.
		age        time.Duration
		defaultTTL time.Duration
		expire     bool
	}{
		{name: "Given no override, when the default TTL elapses, assert that the Session expires",
			policies: []*SessionIdleTTLPolicy{{}}, age: 2 * time.Hour, defaultTTL: time.Hour, expire: true},
		{name: "Given a zero override, when the default TTL elapses, assert that the Session is kept",
			policies: []*SessionIdleTTLPolicy{seconds(0)}, age: 30 * 24 * time.Hour, defaultTTL: time.Hour},
		{name: "Given a short override and a disabled default, when the override elapses, assert that the Session expires",
			policies: []*SessionIdleTTLPolicy{seconds(2)}, age: 3 * time.Second, expire: true},
		{name: "Given a long override, when only the default TTL elapses, assert that the Session is kept",
			policies: []*SessionIdleTTLPolicy{seconds(30 * 24 * 60 * 60)}, age: 8 * 24 * time.Hour, defaultTTL: 7 * 24 * time.Hour},
		{name: "Given an override then an unresolved Harness, when the default TTL elapses, assert that the stored override still applies",
			policies: []*SessionIdleTTLPolicy{seconds(0), nil}, age: 8 * 24 * time.Hour, defaultTTL: 7 * 24 * time.Hour},
		{name: "Given an override then a resolved Harness without one, when the default TTL elapses, assert that the Session expires",
			policies: []*SessionIdleTTLPolicy{seconds(0), {}}, age: 8 * 24 * time.Hour, defaultTTL: 7 * 24 * time.Hour, expire: true},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			client, session := expirationFixture(t)
			for _, policy := range scenario.policies {
				givenHarnessIdleTTL(t, client, policy)
			}
			now := session.CreatedAt.AsTime().Add(scenario.age)

			ids, err := client.ListIdleSessions(t.Context(), now, scenario.defaultTTL, "", 100)
			require.NoError(t, err)
			_, admitErr := client.BeginIdleSessionDeletion(t.Context(), session.Id, now, scenario.defaultTTL)

			// Sessions without events are always listed as candidates; admission
			// checks their creation time, so it decides whether they are kept.
			if scenario.expire {
				require.Equal(t, []string{session.Id}, ids)
				require.NoError(t, admitErr)
			} else {
				require.ErrorIs(t, admitErr, ErrConflict)
			}
		})
	}

	t.Run("Given a scanned candidate, when its Harness disables the TTL before admission, assert that admission rejects it", func(t *testing.T) {
		client, session := expirationFixture(t)
		givenHarnessIdleTTL(t, client, seconds(2))
		now := session.CreatedAt.AsTime().Add(3 * time.Second)
		ids, err := client.ListIdleSessions(t.Context(), now, 0, "", 100)
		require.NoError(t, err)
		require.Equal(t, []string{session.Id}, ids)

		givenHarnessIdleTTL(t, client, seconds(0))
		_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, now, 0)

		require.ErrorIs(t, err, ErrConflict)
	})

	t.Run("Given an admitted idle deletion, when its Harness disables the TTL, assert that the deletion is still retried", func(t *testing.T) {
		client, session := expirationFixture(t)
		givenHarnessIdleTTL(t, client, seconds(2))
		now := session.CreatedAt.AsTime().Add(3 * time.Second)
		_, err := client.BeginIdleSessionDeletion(t.Context(), session.Id, now, 0)
		require.NoError(t, err)

		givenHarnessIdleTTL(t, client, seconds(0))
		ids, err := client.ListIdleSessions(t.Context(), now, 0, "", 100)

		require.NoError(t, err)
		require.Equal(t, []string{session.Id}, ids)
		_, err = client.BeginIdleSessionDeletion(t.Context(), session.Id, now, 0)
		require.NoError(t, err)
	})
}

// givenHarnessIdleTTL reconciles the fixture Agent with a resolved policy, or
// with nil as an unresolved Harness (which also clears the desired revision).
func givenHarnessIdleTTL(t *testing.T, client *Client, policy *SessionIdleTTLPolicy) {
	t.Helper()
	definition := AgentDefinition{Namespace: "team-a", AgentName: "assistant", AgentUID: "assistant-uid", DesiredRevision: "revision-1", SessionIdleTTL: policy}
	if policy == nil {
		definition.DesiredRevision = ""
	}
	require.NoError(t, client.UpsertAgentDefinition(t.Context(), definition))
}
