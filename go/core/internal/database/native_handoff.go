package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"google.golang.org/protobuf/proto"
)

// RequiresWorkspacePreparation derives the profile from the original stored
// Session. Standalone Sessions and Git-free coordinators retain their lifecycle.
func RequiresWorkspacePreparation(session *api.Session) bool {
	return session.GetCreator() == auth.MainloopService && session.GetDevelopmentEnvironment() != nil && session.GetWorkspace() != nil
}

// UsesEnforcingGitProxies reports whether a revision's effective egress reaches
// Git through both Mainloop proxies, the only configuration preparation accepts.
// The compiler adds both origins only for a proxy-mode Git policy; extra proxy
// destinations on a direct-Git revision fail closed into enforcement.
func UsesEnforcingGitProxies(destinations []string) bool {
	return slices.Contains(destinations, apiworkspace.ReadProxyOrigin+":80") && slices.Contains(destinations, apiworkspace.PushProxyOrigin+":80")
}

// RequiresPreparedAdmission gates task admission on a confirmed preparation.
// Direct-Git Sessions keep pre-preparation admission until their Harness
// switches to the enforcing proxies; the revision is server-held and immutable.
func RequiresPreparedAdmission(session *api.Session, revisionDestinations []string) bool {
	return RequiresWorkspacePreparation(session) && UsesEnforcingGitProxies(revisionDestinations)
}

// requiresPreparedAdmission reads the Session's pinned revision egress. A missing
// revision is an error, never an exemption.
func requiresPreparedAdmission(ctx context.Context, db dbExecutor, session *api.Session) (bool, error) {
	if !RequiresWorkspacePreparation(session) {
		return false, nil
	}
	destinations, err := queryOne(ctx, db, `SELECT egress_destinations FROM runtime_revision WHERE revision = $1`, pgx.RowTo[[]string], session.GetPreparedRevision())
	if err != nil {
		return false, fmt.Errorf("read prepared revision egress: %w", notFoundOr(err))
	}
	return RequiresPreparedAdmission(session, destinations), nil
}

// WorkspacePreparationRequired reports whether the runtime must hold turns until
// preparation is confirmed, using the same predicate as store admission.
func (c *Client) WorkspacePreparationRequired(ctx context.Context, session *api.Session) (bool, error) {
	return requiresPreparedAdmission(ctx, c.db, session)
}

type nativePreparationRow struct {
	SessionID           uuid.UUID
	OwnerOperationID    uuid.UUID
	ActionID            string
	RequestDigest       string
	ExecutionID         uuid.UUID
	RequestData         []byte
	AssignmentData      []byte
	Phase               string
	ChallengeID         uuid.UUID
	ObservationSequence int64
	IssuedAt            *time.Time
	ResultData          []byte
	EffectData          []byte
	ReceiptData         []byte
	CurrentValid        bool
}

// readNativePreparation reads original immutable action bytes. Callers hold the
// Session lock for writes; malformed durable records are errors, never omissions.
func readNativePreparation(ctx context.Context, db dbExecutor, sessionID string) (nativePreparationRow, error) {
	return queryOne(ctx, db, `SELECT session_id, owner_operation_id, action_id, request_digest, execution_id,
 request_data, assignment_data, phase, challenge_id, observation_sequence, issued_at,
 result_data, effect_data, receipt_data, current_valid FROM session_native_preparation WHERE session_id = $1`, pgx.RowToStructByName[nativePreparationRow], sessionID)
}

func nativeReceipt(row nativePreparationRow) (*api.WorkspacePreparationReceipt, error) {
	receipt := &api.WorkspacePreparationReceipt{}
	if err := proto.Unmarshal(row.ReceiptData, receipt); err != nil {
		return nil, fmt.Errorf("decode preparation receipt: %w", err)
	}
	request := &api.PrepareSessionWorkspaceRequest{}
	if err := proto.Unmarshal(row.RequestData, request); err != nil {
		return nil, err
	}
	if row.ActionID != request.GetActionId() || row.RequestDigest != workspace.Digest(row.RequestData) || !proto.Equal(request, receipt.GetOriginal()) || receipt.GetExecutionId() != row.ExecutionID.String() || receipt.GetRequestDigest() != row.RequestDigest || receipt.GetChallengeId() != row.ChallengeID.String() || receipt.GetObservationSequence() != uint64(row.ObservationSequence) || receipt.GetClassification() != row.Phase {
		return nil, fmt.Errorf("malformed durable preparation")
	}
	assignment := &api.NativeWorkspacePreparation{}
	if err := proto.Unmarshal(row.AssignmentData, assignment); err != nil {
		return nil, fmt.Errorf("decode preparation assignment: %w", err)
	}
	if assignment.GetSessionId() != row.SessionID.String() || request.GetSessionId() != row.SessionID.String() || assignment.GetActionId() != row.ActionID || assignment.GetCreateRequestId() != request.GetCreateRequestId() || assignment.GetRequestDigest() != row.RequestDigest || assignment.GetExecutionId() != row.ExecutionID.String() || assignment.GetChallengeId() != row.ChallengeID.String() || assignment.GetSequence() != uint64(row.ObservationSequence) || assignment.GetGenerationId() != request.GetGenerationId() || assignment.GetActorUid() != request.GetActorUid() || assignment.GetProfile() != request.GetSetupProfile() || assignment.GetSetupDigest() != request.GetSetupDigest() || assignment.GetContextId() != receipt.GetContextId() || assignment.GetAtespace() != receipt.GetAtespace() || assignment.GetActorName() != receipt.GetActorName() || assignment.GetConfigDigest() != receipt.GetConfigDigest() || assignment.GetMcpDigest() != receipt.GetMcpDigest() || !nativeOriginalSelection(assignment, request) {
		return nil, fmt.Errorf("malformed durable preparation assignment")
	}
	return receipt, nil
}

func nativeOriginalSelection(assignment *api.NativeWorkspacePreparation, request *api.PrepareSessionWorkspaceRequest) bool {
	d, r := request.GetDevelopmentEnvironment(), request.GetRuntimeComposition()
	return assignment.GetPreparedRevision() == request.GetPreparedRevision() && proto.Equal(assignment.GetWorkspace(), request.GetWorkspace()) && assignment.GetDevelopmentImage() == d.GetImage() && assignment.GetPlatform() == d.GetPlatform() && assignment.GetPolicyIdentity() == d.GetPolicyIdentity() && assignment.GetPayloadImage() == r.GetPayloadImage() && assignment.GetProvider() == r.GetProvider() && assignment.GetSchema() == r.GetSchema() && assignment.GetCliVersion() == r.GetCliVersion()
}

func nativeSessionSelection(session *api.Session, request *api.PrepareSessionWorkspaceRequest) bool {
	return RequiresWorkspacePreparation(session) && session.GetId() == request.GetSessionId() && session.GetPreparedRevision() == request.GetPreparedRevision() && proto.Equal(session.GetWorkspace(), request.GetWorkspace()) && proto.Equal(session.GetDevelopmentEnvironment(), request.GetDevelopmentEnvironment()) && proto.Equal(session.GetRuntimeComposition(), request.GetRuntimeComposition())
}

// GetWorkspacePreparationReceipt observes historical evidence, never refreshes
// its times. Missing action means no receipt. Only the authorized Get composes it.
func (c *Client) GetWorkspacePreparationReceipt(ctx context.Context, sessionID string) (*api.WorkspacePreparationReceipt, error) {
	row, err := readNativePreparation(ctx, c.db, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := nativeReceipt(row)
	if err != nil {
		return nil, err
	}
	current, currentErr := readSession(ctx, c.db, sessionID)
	if currentErr != nil || current.State != "RUNTIME_STATE_READY" || current.Operation != "RUNTIME_OPERATION_NONE" || (row.Phase != "confirmed" && (current.OperationID == nil || *current.OperationID != row.OwnerOperationID)) {
		result.Historical = true
	}
	if currentErr == nil {
		session, err := toSession(current)
		if err != nil {
			return nil, err
		}
		if !nativeSessionSelection(session, result.GetOriginal()) {
			result.Historical = true
		}
	}
	generation, genErr := c.GetRuntimeGeneration(ctx, sessionID)
	if genErr != nil || generation.Phase != "active" || generation.ID.String() != result.GetOriginal().GetGenerationId() || generation.ActorUID != result.GetOriginal().GetActorUid() {
		result.Historical = true
	}
	return result, nil
}

// BeginWorkspacePreparation freezes the action before assignment, using the same
// Session operation owner/lock as lifecycle and tasks. Retries join uncertainty;
// completion permits only a fresh read-only challenge against the original tuple.
func (c *Client) BeginWorkspacePreparation(ctx context.Context, input *api.PrepareSessionWorkspaceRequest, assigned *api.NativeWorkspacePreparation, expected *api.Session) (*api.WorkspacePreparationReceipt, error) {
	var result *api.WorkspacePreparationReceipt
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, input.GetSessionId())
		if err != nil {
			return notFoundOr(err)
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		if !SameSessionSelection(session, expected) || session.GetCreator() != expected.GetCreator() || session.GetPreparedRevision() != expected.GetPreparedRevision() || !proto.Equal(session.GetRuntimeComposition(), expected.GetRuntimeComposition()) || !RequiresWorkspacePreparation(session) || session.GetState() != api.RuntimeState_RUNTIME_STATE_READY || session.GetOperation() != api.RuntimeOperation_RUNTIME_OPERATION_NONE || row.OperationID == nil {
			return ErrFailedPrecondition
		}
		original, err := readSessionRequest(ctx, tx, session.GetCreator(), input.GetCreateRequestId())
		if err != nil || original.ID != row.ID || original.SourceCheckpointID != nil {
			return ErrFailedPrecondition
		}
		if !proto.Equal(input.GetWorkspace(), session.GetWorkspace()) || !proto.Equal(input.GetDevelopmentEnvironment(), session.GetDevelopmentEnvironment()) || !proto.Equal(input.GetRuntimeComposition(), session.GetRuntimeComposition()) || input.GetPreparedRevision() != session.GetPreparedRevision() {
			return ErrFailedPrecondition
		}
		generation, err := nativeGeneration(ctx, tx, input.GetSessionId())
		if err != nil || generation.Phase != "active" || generation.ID.String() != input.GetGenerationId() || generation.ActorUID != input.GetActorUid() || generation.ActorUID != assigned.GetActorUid() || generation.ID.String() != assigned.GetGenerationId() {
			return ErrFailedPrecondition
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
		if err != nil || len(raw) > 8192 {
			return ErrFailedPrecondition
		}
		digest := workspace.Digest(raw)
		saved, err := readNativePreparation(ctx, tx, input.GetSessionId())
		if err == nil {
			if saved.ActionID != input.GetActionId() || saved.RequestDigest != digest || !bytes.Equal(saved.RequestData, raw) {
				return ErrIdempotencyConflict
			}
			result, err = nativeReceipt(saved)
			if err != nil {
				return err
			}
			if saved.Phase != "confirmed" {
				if saved.OwnerOperationID != *row.OperationID {
					return ErrConflict
				}
				return nil
			}
			if !saved.CurrentValid || result.GetHistorical() {
				return ErrFailedPrecondition
			}
			if err := admitNativeObservation(ctx, tx, row); err != nil {
				return err
			}
			previous := &api.NativeWorkspacePreparation{}
			if err := proto.Unmarshal(saved.AssignmentData, previous); err != nil {
				return err
			}
			if previous.GetConfigDigest() != assigned.GetConfigDigest() || previous.GetMcpDigest() != assigned.GetMcpDigest() {
				return ErrFailedPrecondition
			}
			if previous.Sequence >= 65535 {
				return ErrFailedPrecondition
			}
			previous.ChallengeId, previous.Sequence = uuid.NewString(), previous.Sequence+1
			data, err := proto.Marshal(previous)
			if err != nil {
				return err
			}
			result.Classification, result.ChallengeId, result.ObservationSequence = "pending", previous.ChallengeId, previous.Sequence
			// Fresh proof remains absent until the challenge commits; old effect time stays.
			result.ObservedAt = nil
			receipt, err := proto.Marshal(result)
			if err != nil {
				return err
			}
			// Only this new read-only challenge adopts the current lifecycle guard.
			// Its original action, execution and effect bytes/timestamp stay unchanged.
			return execSQL(ctx, tx, `UPDATE session_native_preparation SET phase = 'pending', challenge_id = $2,
 observation_sequence = $3, assignment_data = $4, issued_at = NULL, result_data = NULL,
 receipt_data = $5, current_valid = false, owner_operation_id = $6 WHERE session_id = $1`, row.ID, previous.ChallengeId, int64(previous.Sequence), data, receipt, *row.OperationID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := admitNativePreparation(ctx, tx, row); err != nil {
			return err
		}
		assigned = proto.CloneOf(assigned)
		assigned.ActionId, assigned.CreateRequestId, assigned.RequestDigest = input.ActionId, input.CreateRequestId, digest
		assigned.ExecutionId, assigned.ChallengeId, assigned.Sequence = uuid.NewString(), uuid.NewString(), 0
		assigned.Profile, assigned.SetupDigest = input.SetupProfile, input.SetupDigest
		result = &api.WorkspacePreparationReceipt{Original: proto.CloneOf(input), RequestDigest: digest, ExecutionId: assigned.ExecutionId,
			ContextId: session.GetContextId(), Atespace: generation.Atespace, ActorName: generation.ActorName, Classification: "pending", ChallengeId: assigned.ChallengeId,
			ConfigDigest: assigned.ConfigDigest, McpDigest: assigned.McpDigest}
		data, err := proto.Marshal(assigned)
		if err != nil {
			return err
		}
		receipt, err := proto.Marshal(result)
		if err != nil {
			return err
		}
		return execSQL(ctx, tx, `INSERT INTO session_native_preparation (session_id, owner_operation_id, action_id, request_digest, execution_id,
 request_data, assignment_data, phase, challenge_id, observation_sequence, receipt_data)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',$8,0,$9)`, row.ID, *row.OperationID, input.ActionId, digest, assigned.ExecutionId, raw, data, assigned.ChallengeId, receipt)
	})
	return result, err
}

// admitNativePreparation forbids setup after public/native history or concurrent
// checkpoint/dispatch work. It runs only under the owning Session lock.
func admitNativePreparation(ctx context.Context, tx pgx.Tx, row sessionRow) error {
	if err := admitNativeObservation(ctx, tx, row); err != nil {
		return err
	}
	history, err := queryOne(ctx, tx, `SELECT EXISTS(SELECT 1 FROM session_task WHERE history_id = $1)`, pgx.RowTo[bool], row.HistoryID)
	if err != nil {
		return err
	}
	if history {
		return ErrConflict
	}
	return nil
}

// Read-only observations preserve settled native history; active work, HITL,
// dispatch, cleanup and checkpoint creation still exclude a challenge.
func admitNativeObservation(ctx context.Context, tx pgx.Tx, row sessionRow) error {
	if err := requireSettledRuntime(ctx, tx, row.HistoryID, ""); err != nil {
		return err
	}
	busy, err := queryOne(ctx, tx, `SELECT EXISTS(SELECT 1 FROM session_task WHERE history_id = $1
 AND state IN ('TASK_STATE_SUBMITTED', 'TASK_STATE_WORKING', 'TASK_STATE_INPUT_REQUIRED', 'TASK_STATE_AUTH_REQUIRED'))
 OR EXISTS(SELECT 1 FROM session_checkpoint WHERE source_session_id = $2 AND state = 'CREATING')`, pgx.RowTo[bool], row.HistoryID, row.ID)
	if err != nil {
		return err
	}
	if busy {
		return ErrConflict
	}
	return nil
}

func nativeGeneration(ctx context.Context, tx pgx.Tx, sessionID string) (RuntimeGeneration, error) {
	return queryOne(ctx, tx, `SELECT id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at
 FROM runtime_generation WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, pgx.RowToStructByName[RuntimeGeneration], sessionID)
}

// AssignWorkspacePreparation commits the issue marker before returning work.
// A lost response, new connection, or elapsed time cannot issue it again.
func (c *Client) AssignWorkspacePreparation(ctx context.Context, sessionID string) (*api.NativeWorkspacePreparation, bool, error) {
	var assignment *api.NativeWorkspacePreparation
	var ready bool
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return notFoundOr(err)
		}
		saved, err := readNativePreparation(ctx, tx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		receipt, err := nativeReceipt(saved)
		if err != nil {
			return err
		}
		generation, err := nativeGeneration(ctx, tx, sessionID)
		if err != nil || generation.Phase != "active" || generation.ID.String() != receipt.GetOriginal().GetGenerationId() || generation.ActorUID != receipt.GetOriginal().GetActorUid() || row.Operation != "RUNTIME_OPERATION_NONE" || row.State != "RUNTIME_STATE_READY" {
			return ErrConflict
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		if !nativeSessionSelection(session, receipt.GetOriginal()) || (saved.Phase != "confirmed" && (row.OperationID == nil || *row.OperationID != saved.OwnerOperationID)) {
			return ErrConflict
		}
		ready = saved.CurrentValid && saved.Phase == "confirmed"
		if saved.Phase != "pending" {
			return nil
		}
		assignment = &api.NativeWorkspacePreparation{}
		if err := proto.Unmarshal(saved.AssignmentData, assignment); err != nil {
			return err
		}
		receipt.Classification = "uncertain"
		raw, err := proto.Marshal(receipt)
		if err != nil {
			return err
		}
		return execSQL(ctx, tx, `UPDATE session_native_preparation SET phase = 'uncertain', issued_at = clock_timestamp(), receipt_data = $2 WHERE session_id = $1 AND phase = 'pending'`, row.ID, raw)
	})
	return assignment, ready, err
}

// CompleteWorkspacePreparation commits only the originally assigned result.
// Already authenticated in-flight callbacks may settle history after revocation;
// the locked generation/operation check prevents restoration of current admission.
func (c *Client) CompleteWorkspacePreparation(ctx context.Context, binding RuntimeGeneration, observedCurrent bool, input *api.TaskStoreServiceCompleteWorkspacePreparationRequest) error {
	return c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, input.GetSessionId())
		if err != nil {
			return notFoundOr(err)
		}
		saved, err := readNativePreparation(ctx, tx, input.GetSessionId())
		if err != nil {
			return notFoundOr(err)
		}
		assignment := &api.NativeWorkspacePreparation{}
		if err := proto.Unmarshal(saved.AssignmentData, assignment); err != nil {
			return err
		}
		if !proto.Equal(assignment, input.GetAssignment()) || binding.SessionID.String() != assignment.GetSessionId() || binding.ID.String() != assignment.GetGenerationId() || binding.ActorUID != assignment.GetActorUid() || binding.Atespace != assignment.GetAtespace() || binding.ActorName != assignment.GetActorName() {
			return ErrFailedPrecondition
		}
		generation, err := nativeGeneration(ctx, tx, input.GetSessionId())
		if err != nil || generation.ID != binding.ID || generation.ActorUID != binding.ActorUID || generation.CredentialURI != binding.CredentialURI || !bytes.Equal(generation.TokenDigest, binding.TokenDigest) {
			return ErrFailedPrecondition
		}
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
		if err != nil || len(data) > 16384 {
			return ErrFailedPrecondition
		}
		if saved.ResultData != nil {
			if !bytes.Equal(saved.ResultData, data) {
				return ErrIdempotencyConflict
			}
			return nil
		}
		if saved.Phase != "uncertain" || saved.IssuedAt == nil || input.GetObservedAt() == nil || input.ObservedAt.CheckValid() != nil || input.ObservedAt.AsTime().Before(*saved.IssuedAt) || input.ObservedAt.AsTime().After(time.Now().Add(time.Minute)) {
			return ErrFailedPrecondition
		}
		receipt, err := nativeReceipt(saved)
		if err != nil {
			return err
		}
		if input.GetConfirmed() {
			expectedHook := "developer_instruction"
			if assignment.GetProvider() == "claude" {
				expectedHook = "append_system_prompt"
			}
			policy := workspaceProxyPolicy()
			expectedTransport, err := workspace.TransportDigest(policy, assignment.GetWorkspace().GetRepo())
			if err != nil || input.GetHead() != assignment.GetWorkspace().GetRef() || input.GetBranch() != assignment.GetWorkspace().GetBranch() || input.GetTransportDigest() != expectedTransport || input.GetNativeHook() != expectedHook {
				return ErrFailedPrecondition
			}
			receipt.Classification = "confirmed"
		} else {
			receipt.Classification = "definite-failure"
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		current := observedCurrent && nativeSessionSelection(session, receipt.GetOriginal()) && generation.Phase == "active" && row.State == "RUNTIME_STATE_READY" && row.Operation == "RUNTIME_OPERATION_NONE" && row.OperationID != nil && *row.OperationID == saved.OwnerOperationID
		receipt.Historical = !current
		receipt.Head, receipt.Branch, receipt.TransportDigest, receipt.NativeHook, receipt.ObservedAt = input.Head, input.Branch, input.TransportDigest, input.NativeHook, input.ObservedAt
		if assignment.Sequence == 0 {
			receipt.EffectObservedAt = input.ObservedAt
		}
		raw, err := proto.Marshal(receipt)
		if err != nil || len(raw) > 16384 {
			return ErrFailedPrecondition
		}
		return execSQL(ctx, tx, `UPDATE session_native_preparation SET phase = $2, result_data = $3,
 effect_data = CASE WHEN observation_sequence = 0 THEN $3 ELSE effect_data END, receipt_data = $4,
 current_valid = $5 WHERE session_id = $1`, row.ID, receipt.Classification, data, raw, current && input.Confirmed)
	})
}

// requireNativeTaskAdmission positively checks owned code preparations. It never
// imposes the Mainloop-only API on an ordinary standalone, Git-free or direct-Git
// Session.
func requireNativeTaskAdmission(ctx context.Context, tx pgx.Tx, row sessionRow) error {
	session, err := toSession(row)
	if err != nil {
		return err
	}
	required, err := requiresPreparedAdmission(ctx, tx, session)
	if err != nil || !required {
		return err
	}
	saved, err := readNativePreparation(ctx, tx, row.ID.String())
	// Confirmed setup belongs to the captured runtime generation, not a completed
	// Suspend/Resume operation. Pending effects keep their operation guard above.
	if err != nil || saved.Phase != "confirmed" || !saved.CurrentValid {
		return fmt.Errorf("workspace preparation is incomplete: %w", ErrFailedPrecondition)
	}
	receipt, err := nativeReceipt(saved)
	if err != nil {
		return err
	}
	if !nativeSessionSelection(session, receipt.GetOriginal()) {
		return ErrFailedPrecondition
	}
	generation, err := nativeGeneration(ctx, tx, row.ID.String())
	if err != nil || generation.Phase != "active" || generation.ID.String() != receipt.GetOriginal().GetGenerationId() || generation.ActorUID != receipt.GetOriginal().GetActorUid() {
		return ErrFailedPrecondition
	}
	return nil
}

func workspaceProxyPolicy() apiworkspace.Git {
	return apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
}
