package database

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
	"github.com/kagent-dev/kagent/go/core/pkg/migrations"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nativeActionFixture(t *testing.T) (*Client, *Client, *api.Session, *RuntimeGeneration, *api.PrepareSessionWorkspaceRequest, *api.NativeWorkspacePreparation) {
	t.Helper()
	return nativeActionFixtureWithEgress(t, []string{apiworkspace.ReadProxyOrigin + ":80", apiworkspace.PushProxyOrigin + ":80"})
}

// nativeActionFixtureWithEgress pins the composed revision's effective egress,
// which decides whether its Git uses the enforcing proxies.
func nativeActionFixtureWithEgress(t *testing.T, destinations []string) (*Client, *Client, *api.Session, *RuntimeGeneration, *api.PrepareSessionWorkspaceRequest, *api.NativeWorkspacePreparation) {
	t.Helper()
	client := NewClient(setupTestDB(t))
	peerPool, err := pgxpool.New(t.Context(), sharedConnStr)
	require.NoError(t, err)
	t.Cleanup(peerPool.Close)
	peer := NewClient(peerPool)
	sessionFixture(t, client, t.Context(), "team-a", "base-native", "assistant", "codex")
	base, err := client.GetRuntimeRevision(t.Context(), "base-native")
	require.NoError(t, err)
	req := newSessionRequest(uuid.NewString(), "assistant", "codex", "")
	req.Creator = "mainloop"
	req.Workspace = &api.Workspace{Repo: "https://github.com/owner/repo.git", Ref: strings.Repeat("a", 40), Branch: "feature-native", Depth: 1}
	req.DevelopmentEnvironment = &api.DevelopmentEnvironment{Image: "fixture/d@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64", PolicyIdentity: "fixture-v1"}
	req.RuntimeComposition = &api.RuntimeComposition{PayloadImage: "fixture/r@sha256:" + strings.Repeat("b", 64), Provider: "codex", Schema: 1, CliVersion: "1.0"}
	req.PreparedRevision = "composed-native"
	variant := *base
	variant.Revision, variant.ActorTemplateName, variant.ActorTemplateUID = "composed-native", "composed-native-template", "composed-native-uid"
	variant.SourceSnapshot, err = json.Marshal(EnvironmentRevisionSnapshot{BaseRevision: "base-native", Environment: req.DevelopmentEnvironment, Composition: req.RuntimeComposition})
	require.NoError(t, err)
	variant.GitOrigins = []string{"github.com"}
	variant.EgressDestinations = destinations
	require.NoError(t, client.RecordRuntimeRevision(t.Context(), variant, false))
	session, _, err := client.CreateSession(t.Context(), req, "native-create")
	require.NoError(t, err)
	g, allocated, err := client.AllocateRuntimeGeneration(t.Context(), generationCandidate(session.Id))
	require.NoError(t, err)
	require.True(t, allocated)
	activateGeneration(t, client, g)
	session, err = finishSessionOperation(t.Context(), client, session.Id, api.RuntimeOperation_RUNTIME_OPERATION_CREATE, "runtime.example")
	require.NoError(t, err)
	setup, err := workspace.SetupDigest("child")
	require.NoError(t, err)
	input := &api.PrepareSessionWorkspaceRequest{SessionId: session.Id, ActionId: "native-create:prepare", CreateRequestId: "native-create", GenerationId: g.ID.String(), ActorUid: g.ActorUID, PreparedRevision: session.PreparedRevision, Workspace: proto.CloneOf(session.Workspace), DevelopmentEnvironment: proto.CloneOf(session.DevelopmentEnvironment), RuntimeComposition: proto.CloneOf(session.RuntimeComposition), SetupProfile: "child", SetupDigest: setup}
	assignment := &api.NativeWorkspacePreparation{SessionId: session.Id, ContextId: session.ContextId, GenerationId: g.ID.String(), Atespace: g.Atespace, ActorName: g.ActorName, ActorUid: g.ActorUID, PreparedRevision: session.PreparedRevision, Workspace: proto.CloneOf(session.Workspace), DevelopmentImage: req.DevelopmentEnvironment.Image, Platform: req.DevelopmentEnvironment.Platform, PolicyIdentity: req.DevelopmentEnvironment.PolicyIdentity, PayloadImage: req.RuntimeComposition.PayloadImage, Provider: "codex", Schema: 1, CliVersion: "1.0", ConfigDigest: workspace.Digest([]byte("compiler")), McpDigest: workspace.Digest([]byte("mcp"))}
	return client, peer, session, g, input, assignment
}

func nativeCompletion(t *testing.T, assignment *api.NativeWorkspacePreparation) *api.TaskStoreServiceCompleteWorkspacePreparationRequest {
	t.Helper()
	digest, err := workspace.TransportDigest(workspaceProxyPolicy(), assignment.Workspace.Repo)
	require.NoError(t, err)
	return &api.TaskStoreServiceCompleteWorkspacePreparationRequest{SessionId: assignment.SessionId, Assignment: proto.CloneOf(assignment), Head: assignment.Workspace.Ref, Branch: assignment.Workspace.Branch, TransportDigest: digest, NativeHook: "developer_instruction", ObservedAt: timestamppb.Now(), Confirmed: true}
}

func TestNativePreparationIssueCASAndAdmission(t *testing.T) {
	c, peer, session, g, input, prepared := nativeActionFixture(t)
	require.ErrorIs(t, c.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""), ErrFailedPrecondition)
	receipt, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	require.Equal(t, "pending", receipt.Classification)
	require.Nil(t, receipt.EffectObservedAt)
	for _, kind := range []api.RuntimeOperation{api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, api.RuntimeOperation_RUNTIME_OPERATION_DELETE} {
		_, err := peer.BeginSessionOperation(t.Context(), session.Id, kind)
		require.Error(t, err)
	}
	firstConn, err := sharedDB.Acquire(t.Context())
	require.NoError(t, err)
	secondConn, err := sharedDB.Acquire(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, firstConn.Conn().PgConn().PID(), secondConn.Conn().PgConn().PID())
	firstConn.Release()
	secondConn.Release()
	var wg sync.WaitGroup
	assignments := make(chan *api.NativeWorkspacePreparation, 2)
	failures := make(chan error, 2)
	for _, store := range []*Client{c, peer} {
		wg.Go(func() {
			a, _, err := store.AssignWorkspacePreparation(t.Context(), session.Id)
			assignments <- a
			failures <- err
		})
	}
	wg.Wait()
	close(assignments)
	close(failures)
	var assigned *api.NativeWorkspacePreparation
	for err := range failures {
		require.NoError(t, err)
	}
	for a := range assignments {
		if a != nil {
			require.Nil(t, assigned, "only one connection may issue")
			assigned = a
		}
	}
	require.NotNil(t, assigned)
	// Lost assignment response/restart can only observe uncertainty.
	other, ready, err := peer.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	require.Nil(t, other)
	require.False(t, ready)
	retried, err := peer.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	require.Equal(t, receipt.ExecutionId, retried.ExecutionId)
	require.Equal(t, "uncertain", retried.Classification)
	changed := proto.CloneOf(input)
	changed.SetupProfile = "supervisor"
	_, err = c.BeginWorkspacePreparation(t.Context(), changed, prepared, session)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	result := nativeCompletion(t, assigned)
	require.NoError(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, result))
	require.NoError(t, peer.CompleteWorkspacePreparation(t.Context(), *g, true, result), "lost receipt reply retries same bytes")
	changedResult := proto.CloneOf(result)
	changedResult.Head = strings.Repeat("b", 40)
	require.ErrorIs(t, peer.CompleteWorkspacePreparation(t.Context(), *g, true, changedResult), ErrIdempotencyConflict)
	effect, err := c.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, "confirmed", effect.Classification)
	require.False(t, effect.Historical)
	challenge, err := peer.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	require.Equal(t, uint64(1), challenge.ObservationSequence)
	require.True(t, proto.Equal(effect.EffectObservedAt, challenge.EffectObservedAt))
	require.Nil(t, challenge.ObservedAt)
	require.ErrorIs(t, c.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""), ErrFailedPrecondition)
	observed, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, assigned.ExecutionId, observed.ExecutionId)
	require.NoError(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, nativeCompletion(t, observed)))
	require.NoError(t, c.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""))
}

func TestNativePreparationLateReceiptIsHistorical(t *testing.T) {
	c, peer, session, g, input, prepared := nativeActionFixture(t)
	_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	assigned, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	result := nativeCompletion(t, assigned)
	require.NoError(t, peer.RevokeRuntimeGeneration(t.Context(), session.Id))
	require.NoError(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, result))
	receipt, err := peer.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.True(t, receipt.Historical)
	require.Equal(t, "confirmed", receipt.Classification)
	require.ErrorIs(t, peer.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""), ErrFailedPrecondition)
	_, err = peer.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.Error(t, err)
}

func TestNativePreparationLifecyclePreservesHistoryAndObservation(t *testing.T) {
	c, peer, session, g, input, prepared := nativeActionFixture(t)
	_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	assigned, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	original := nativeCompletion(t, assigned)
	require.NoError(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, original))
	for _, kind := range []api.RuntimeOperation{api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND, api.RuntimeOperation_RUNTIME_OPERATION_RESUME} {
		session, err = finishSessionOperation(t.Context(), peer, session.Id, kind, "runtime.example")
		require.NoError(t, err)
	}
	loaded, ready, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	require.Nil(t, loaded)
	require.True(t, ready)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("existing continuation"))
	message.ContextID = session.ContextId
	task := a2a.NewSubmittedTask(message, message)
	require.NoError(t, saveRuntimeTask(t, c, session.Id, task, task, nil), "new current write after Resume")
	_, err = peer.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.Error(t, err, "active native work must still exclude observation")
	task.Status.State = a2a.TaskStateCompleted
	require.NoError(t, saveRuntimeTask(t, c, session.Id, task, task, &SessionTaskSnapshot{Atespace: g.Atespace, URI: "fixture://native-history", ContentScope: "DATA"}))
	before, err := c.GetSessionTask(t.Context(), session.Id, string(task.ID), nil)
	require.NoError(t, err)
	challenge, err := peer.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err, "settled history permits read-only revalidation")
	require.Equal(t, assigned.ExecutionId, challenge.ExecutionId)
	require.True(t, proto.Equal(original.ObservedAt, challenge.EffectObservedAt))
	observed, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, uint64(1), observed.Sequence)
	_, err = peer.BeginSessionOperation(t.Context(), session.Id, api.RuntimeOperation_RUNTIME_OPERATION_SUSPEND)
	require.Error(t, err, "pending observation still serializes with lifecycle")
	require.NoError(t, peer.CompleteWorkspacePreparation(t.Context(), *g, true, nativeCompletion(t, observed)))
	after, err := c.GetSessionTask(t.Context(), session.Id, string(task.ID), nil)
	require.NoError(t, err)
	require.Equal(t, before, after, "observation must preserve native history")
	receipt, err := c.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.False(t, receipt.Historical)
	require.True(t, proto.Equal(original.ObservedAt, receipt.EffectObservedAt))
	require.NoError(t, peer.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""))
}

func TestNativePreparationReceiptWaitsForSessionOwner(t *testing.T) {
	c, peer, session, g, input, prepared := nativeActionFixture(t)
	_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	assigned, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	result := nativeCompletion(t, assigned)
	tx, err := sharedDB.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, err = lockSession(t.Context(), tx, session.Id)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- peer.CompleteWorkspacePreparation(t.Context(), *g, true, result) }()
	// Observe an actual blocked backend; elapsed time alone cannot prove locking.
	require.Eventually(t, func() bool {
		var blocked bool
		err := sharedDB.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	// The competing owner's committed revoke wins before the receipt obtains the lock.
	_, err = tx.Exec(t.Context(), `UPDATE runtime_generation SET phase = 'revoked' WHERE session_id = $1`, session.Id)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	require.NoError(t, <-done)
	receipt, err := c.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.True(t, receipt.Historical)
	require.ErrorIs(t, c.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), ""), ErrFailedPrecondition)
}

func TestNativePreparationRejectsCorruptDurableAssignment(t *testing.T) {
	c, _, session, _, input, prepared := nativeActionFixture(t)
	_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	assigned, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	assigned.DevelopmentImage = "foreign"
	raw, err := proto.Marshal(assigned)
	require.NoError(t, err)
	_, err = sharedDB.Exec(t.Context(), `UPDATE session_native_preparation SET assignment_data=$2 WHERE session_id=$1`, session.Id, raw)
	require.NoError(t, err)
	_, err = c.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.ErrorContains(t, err, "malformed durable preparation assignment")
	_, _, err = c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.Error(t, err)
}

func TestNativePreparationRejectsMalformedOriginalAndResult(t *testing.T) {
	for _, name := range []string{"create", "D", "R", "revision", "generation", "UID", "wrong HEAD", "wrong transport", "wrong setup", "unknown challenge"} {
		t.Run(name, func(t *testing.T) {
			c, _, session, g, input, prepared := nativeActionFixture(t)
			switch name {
			case "create":
				input.CreateRequestId = "other"
			case "D":
				input.DevelopmentEnvironment.Image = "other"
			case "R":
				input.RuntimeComposition.PayloadImage = "other"
			case "revision":
				input.PreparedRevision = "other"
			case "generation":
				input.GenerationId = uuid.NewString()
			case "UID":
				input.ActorUid = "other"
			}
			_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
			if name == "create" || name == "D" || name == "R" || name == "revision" || name == "generation" || name == "UID" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assigned, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
			require.NoError(t, err)
			result := nativeCompletion(t, assigned)
			switch name {
			case "wrong HEAD":
				result.Head = strings.Repeat("b", 40)
			case "wrong transport":
				result.TransportDigest = workspace.Digest([]byte("wrong"))
			case "wrong setup":
				result.Assignment.SetupDigest = workspace.Digest([]byte("wrong"))
			case "unknown challenge":
				result.Assignment.ChallengeId = uuid.NewString()
			}
			require.Error(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, result))
			receipt, err := c.GetWorkspacePreparationReceipt(t.Context(), session.Id)
			require.NoError(t, err)
			require.Equal(t, "uncertain", receipt.Classification)
		})
	}
}

func TestNativePreparationPreservesOrdinaryTaskAdmission(t *testing.T) {
	c := NewClient(setupTestDB(t))
	sessionFixture(t, c, t.Context(), "team-a", "ordinary", "assistant", "codex")
	for _, creator := range []string{"alice", "mainloop"} {
		req := newSessionRequest(uuid.NewString(), "assistant", "codex", "")
		req.Creator = creator
		session, _, err := c.CreateSession(t.Context(), req, uuid.NewString())
		require.NoError(t, err)
		session, err = finishSessionOperation(t.Context(), c, session.Id, api.RuntimeOperation_RUNTIME_OPERATION_CREATE, "runtime.example")
		require.NoError(t, err)
		message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ordinary prompt"))
		message.ContextID = session.ContextId
		task := a2a.NewSubmittedTask(message, message)
		_, err = c.CreateRuntimeTask(t.Context(), session.Id, make([]byte, 32), task, "")
		require.NoError(t, err, "standalone and Git-free coordinator retain positive native task behavior")
	}
}

func TestNativePreparationReceiptCommitFailureDoesNotReissue(t *testing.T) {
	c, peer, session, g, input, prepared := nativeActionFixture(t)
	_, err := c.BeginWorkspacePreparation(t.Context(), input, prepared, session)
	require.NoError(t, err)
	assignment, _, err := c.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	result := nativeCompletion(t, assignment)
	// Inject a real PostgreSQL receipt-commit rejection, rather than a fake service answer.
	_, err = sharedDB.Exec(t.Context(), `ALTER TABLE session_native_preparation ADD CONSTRAINT reject_native_receipt CHECK (phase <> 'confirmed') NOT VALID`)
	require.NoError(t, err)
	dropped := false
	defer func() {
		if !dropped {
			_, err := sharedDB.Exec(context.Background(), `ALTER TABLE session_native_preparation DROP CONSTRAINT reject_native_receipt`)
			require.NoError(t, err)
		}
	}()
	require.Error(t, c.CompleteWorkspacePreparation(t.Context(), *g, true, result))
	receipt, err := peer.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.Equal(t, "uncertain", receipt.Classification)
	retry, ready, err := peer.AssignWorkspacePreparation(t.Context(), session.Id)
	require.NoError(t, err)
	require.Nil(t, retry)
	require.False(t, ready)
	_, err = sharedDB.Exec(t.Context(), `ALTER TABLE session_native_preparation DROP CONSTRAINT reject_native_receipt`)
	require.NoError(t, err)
	dropped = true
	require.NoError(t, peer.CompleteWorkspacePreparation(t.Context(), *g, true, result))
	// The original effect time survives the failed transaction.
	receipt, err = peer.GetWorkspacePreparationReceipt(t.Context(), session.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(result.ObservedAt, receipt.EffectObservedAt))
}

func TestNativePreparationMigrationReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	dsn := dbtest.StartT(context.WithoutCancel(t.Context()), t)
	require.NoError(t, migrations.RunUp(t.Context(), dsn, migrations.BuiltinSources(false)))
	require.NoError(t, migrations.RunUp(t.Context(), dsn, migrations.BuiltinSources(false)))
	require.NoError(t, migrations.WithProvider(t.Context(), dsn, migrations.BuiltinSources(false)[0], func(p *goose.Provider) error {
		result, err := p.DownTo(t.Context(), 5)
		require.NoError(t, err)
		require.NotEmpty(t, result)
		require.Equal(t, int64(6), result[len(result)-1].Source.Version)
		return nil
	}))
	require.NoError(t, migrations.RunUp(t.Context(), dsn, migrations.BuiltinSources(false)))
	require.NoError(t, migrations.VerifyMigrated(t.Context(), dsn, migrations.BuiltinSources(false)))
}

func TestUsesEnforcingGitProxies(t *testing.T) {
	read, push := apiworkspace.ReadProxyOrigin+":80", apiworkspace.PushProxyOrigin+":80"
	for name, tc := range map[string]struct {
		destinations []string
		want         bool
	}{
		"none":         {nil, false},
		"direct Git":   {[]string{"https://github.com:443"}, false},
		"read only":    {[]string{read}, false},
		"push only":    {[]string{push}, false},
		"both proxies": {[]string{"http://mainloop-mcp.mainloop.svc.cluster.local:80", read, push}, true},
		"bare proxies": {[]string{apiworkspace.ReadProxyOrigin, apiworkspace.PushProxyOrigin}, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, UsesEnforcingGitProxies(tc.destinations))
		})
	}
}

func TestNativeAdmissionRequiresPreparationOnlyForProxyGit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		destinations []string
		required     bool
	}{
		{"direct Git", []string{"https://github.com:443"}, false},
		{"read proxy only", []string{apiworkspace.ReadProxyOrigin + ":80"}, false},
		{"enforcing proxies", []string{apiworkspace.ReadProxyOrigin + ":80", apiworkspace.PushProxyOrigin + ":80"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, session := nativeSessionWithEgress(t, tc.destinations)
			require.True(t, RequiresWorkspacePreparation(session), "fixture is a Mainloop D + workspace Session")
			required, err := c.WorkspacePreparationRequired(t.Context(), session)
			require.NoError(t, err)
			require.Equal(t, tc.required, required)
			err = c.ReserveSessionDispatch(t.Context(), session.Id, uuid.New(), "")
			if tc.required {
				require.ErrorIs(t, err, ErrFailedPrecondition, "proxy-mode admission waits for confirmed preparation")
			} else {
				require.NoError(t, err, "direct-Git admission behaves as before preparation")
			}
		})
	}
}

func TestNativeAdmissionRejectsMissingRevision(t *testing.T) {
	c, session := nativeSessionWithEgress(t, []string{apiworkspace.ReadProxyOrigin + ":80", apiworkspace.PushProxyOrigin + ":80"})
	missing := proto.CloneOf(session)
	missing.PreparedRevision = "absent-revision"
	_, err := c.WorkspacePreparationRequired(t.Context(), missing)
	require.ErrorIs(t, err, ErrNotFound)
}

// nativeSessionWithEgress keeps only the store and Session of a native fixture.
func nativeSessionWithEgress(t *testing.T, destinations []string) (*Client, *api.Session) {
	t.Helper()
	c, _, session, _, _, _ := nativeActionFixtureWithEgress(t, destinations) //nolint:dogsled // admission needs only the Session
	return c, session
}
