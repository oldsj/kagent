package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
)

// Preparation phases, which are also the receipt classifications.
const (
	PreparationPending         = "pending"
	PreparationConfirmed       = "confirmed"
	PreparationDefiniteFailure = "definite-failure"
)

// RequiresWorkspacePreparation reports whether turns wait for a confirmed
// preparation: the Session runs a selected development environment, asked for
// a workspace, and its revision reaches Git only through the trusted proxies.
func RequiresWorkspacePreparation(session *apiv1.Session, revisionConfig []byte) bool {
	if session.GetDevelopmentEnvironment() == nil || session.GetWorkspace() == nil {
		return false
	}
	var config struct {
		Git *apiworkspace.Git `json:"git"`
	}
	if json.Unmarshal(revisionConfig, &config) != nil || config.Git == nil {
		return false
	}
	return config.Git.ReadProxyOrigin != nil && config.Git.PushProxyOrigin != nil
}

// PreparationRequestDigest is the SHA-256 of the request's deterministic encoding.
func PreparationRequestDigest(request *apiv1.PrepareSessionWorkspaceRequest) ([]byte, string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

// PreparationMatchesSession reports whether a request names exactly the
// Session's own revision, workspace, environment and composition.
func PreparationMatchesSession(request *apiv1.PrepareSessionWorkspaceRequest, session *apiv1.Session) bool {
	return request.GetSessionId() == session.GetId() && request.GetPreparedRevision() == session.GetPreparedRevision() &&
		proto.Equal(request.GetWorkspace(), session.GetWorkspace()) &&
		proto.Equal(request.GetDevelopmentEnvironment(), session.GetDevelopmentEnvironment()) &&
		proto.Equal(request.GetRuntimeComposition(), session.GetRuntimeComposition())
}

type preparationRow struct {
	ActionID         string
	RequestDigest    string
	RequestData      []byte
	ExecutionID      uuid.UUID
	AssignmentData   []byte
	Phase            string
	IssuedAt         *time.Time
	ResultData       []byte
	EffectObservedAt *time.Time
	SettledAt        *time.Time
}

type preparationRecord struct {
	row        preparationRow
	request    *apiv1.PrepareSessionWorkspaceRequest
	assignment *runtimev1.WorkspacePreparation
	result     *runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest
}

// decodePreparation rejects malformed durable bytes rather than omitting them.
func decodePreparation(row preparationRow) (*preparationRecord, error) {
	record := &preparationRecord{row: row, request: &apiv1.PrepareSessionWorkspaceRequest{}, assignment: &runtimev1.WorkspacePreparation{}}
	if err := proto.Unmarshal(row.RequestData, record.request); err != nil {
		return nil, fmt.Errorf("decode preparation request: %w", err)
	}
	if err := proto.Unmarshal(row.AssignmentData, record.assignment); err != nil {
		return nil, fmt.Errorf("decode preparation assignment: %w", err)
	}
	if row.ResultData != nil {
		record.result = &runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest{}
		if err := proto.Unmarshal(row.ResultData, record.result); err != nil {
			return nil, fmt.Errorf("decode preparation result: %w", err)
		}
	}
	return record, nil
}

// preparationReceipt reports a record as evidence. currentActorUID is the
// Session's recorded Actor; a receipt for any other Actor is historical.
func preparationReceipt(record *preparationRecord, currentActorUID string) *apiv1.WorkspacePreparationReceipt {
	assignment := record.assignment
	receipt := &apiv1.WorkspacePreparationReceipt{
		Original: record.request, RequestDigest: record.row.RequestDigest, ExecutionId: record.row.ExecutionID.String(),
		ContextId: assignment.GetContextId(), Atespace: assignment.GetAtespace(), ActorName: assignment.GetActorName(),
		Classification: record.row.Phase, ChallengeId: assignment.GetChallengeId(), ObservationSequence: assignment.GetSequence(),
		ConfigDigest: assignment.GetConfigDigest(), McpDigest: assignment.GetMcpDigest(),
		Historical: assignment.GetActorUid() != currentActorUID,
	}
	if record.row.Phase == PreparationConfirmed && record.result != nil {
		receipt.Head, receipt.Branch = record.result.GetHead(), record.result.GetBranch()
		receipt.TransportDigest, receipt.NativeHook = record.result.GetTransportDigest(), record.result.GetNativeHook()
	}
	if record.row.EffectObservedAt != nil {
		receipt.EffectObservedAt = timestamppb.New(*record.row.EffectObservedAt)
	}
	if record.row.SettledAt != nil {
		receipt.ObservedAt = timestamppb.New(*record.row.SettledAt)
	}
	return receipt
}

func readPreparation(ctx context.Context, db dbExecutor, sessionID string) (*preparationRecord, error) {
	row, err := queryOne(ctx, db, `
		SELECT action_id, request_digest, request_data, execution_id, assignment_data, phase,
		    issued_at, result_data, effect_observed_at, settled_at
		FROM session_workspace_preparation WHERE session_id = $1
	`, pgx.RowToStructByName[preparationRow], sessionID)
	if err != nil {
		return nil, err
	}
	return decodePreparation(row)
}

func sessionActorUID(ctx context.Context, db dbExecutor, sessionID string) (string, error) {
	uid, err := queryOne(ctx, db, `SELECT COALESCE(actor_uid, '') FROM session_record WHERE id = $1`, pgx.RowTo[string], sessionID)
	return uid, notFoundOr(err)
}

func revisionConfig(ctx context.Context, db dbExecutor, revision string) ([]byte, error) {
	raw, err := queryOne(ctx, db, `SELECT bootstrap_bundle FROM agent_revision WHERE revision = $1`, pgx.RowTo[[]byte], revision)
	if err != nil {
		return nil, fmt.Errorf("read revision %s bootstrap bundle: %w", revision, notFoundOr(err))
	}
	if raw == nil {
		return nil, nil
	}
	bundle := &runtimev1.BootstrapBundle{}
	if err := proto.Unmarshal(raw, bundle); err != nil {
		return nil, fmt.Errorf("decode revision %s bootstrap bundle: %w", revision, err)
	}
	return bundle.GetConfigJson(), nil
}

// GetRevisionConfig returns a revision's compiled bootstrap config JSON, or nil
// when the revision has no published bundle.
func (c *Client) GetRevisionConfig(ctx context.Context, revision string) ([]byte, error) {
	return revisionConfig(ctx, c.db, revision)
}

// sessionRequiresPreparation reads the pinned revision only for Sessions that
// could require preparation. A missing revision is an error, never an exemption.
func sessionRequiresPreparation(ctx context.Context, db dbExecutor, session *apiv1.Session) (bool, error) {
	if session.GetDevelopmentEnvironment() == nil || session.GetWorkspace() == nil {
		return false, nil
	}
	config, err := revisionConfig(ctx, db, session.GetPreparedRevision())
	if err != nil {
		return false, err
	}
	return RequiresWorkspacePreparation(session, config), nil
}

// BeginWorkspacePreparation records a preparation for expected, the Session the
// caller authorized and checked. Under the Session lock it verifies the Session
// is unchanged and idle, the request names its creation request and current
// Actor, and no other preparation exists. A retry with identical bytes returns
// the recorded receipt; other bytes return ErrIdempotencyConflict. A changed
// Session or Actor returns ErrFailedPrecondition.
func (c *Client) BeginWorkspacePreparation(ctx context.Context, request *apiv1.PrepareSessionWorkspaceRequest, assignment *runtimev1.WorkspacePreparation, expected *apiv1.Session) (*apiv1.WorkspacePreparationReceipt, error) {
	raw, digest, err := PreparationRequestDigest(request)
	if err != nil {
		return nil, err
	}
	var receipt *apiv1.WorkspacePreparationReceipt
	err = c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, request.GetSessionId())
		if err != nil {
			return notFoundOr(err)
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		actorUID, err := sessionActorUID(ctx, tx, session.GetId())
		if err != nil {
			return err
		}
		saved, err := readPreparation(ctx, tx, session.GetId())
		if err == nil {
			if saved.row.ActionID != request.GetActionId() || saved.row.RequestDigest != digest {
				return ErrIdempotencyConflict
			}
			receipt = preparationReceipt(saved, actorUID)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if session.GetState() != apiv1.RuntimeState_RUNTIME_STATE_READY || session.GetOperation() != apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE ||
			session.GetCreator() != expected.GetCreator() || !PreparationMatchesSession(request, session) {
			return ErrFailedPrecondition
		}
		if actorUID == "" || actorUID != request.GetActorUid() || actorUID != assignment.GetActorUid() {
			return ErrFailedPrecondition
		}
		original, err := readSessionRequest(ctx, tx, session.GetCreator(), request.GetCreateRequestId())
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (original.ID.String() != session.GetId() || original.SourceCheckpointID != nil)) {
			return ErrFailedPrecondition
		}
		if err != nil {
			return err
		}
		required, err := sessionRequiresPreparation(ctx, tx, session)
		if err != nil {
			return err
		}
		if !required {
			return ErrFailedPrecondition
		}
		assignment = proto.CloneOf(assignment)
		assignment.RequestDigest = digest
		assignment.ExecutionId = uuid.NewString()
		assignment.ChallengeId = uuid.NewString()
		assignment.Sequence = 0
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(assignment)
		if err != nil {
			return err
		}
		if err := execSQL(ctx, tx, `
			INSERT INTO session_workspace_preparation (session_id, action_id, request_digest, request_data, execution_id, assignment_data, phase)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		`, session.GetId(), request.GetActionId(), digest, raw, assignment.GetExecutionId(), data); err != nil {
			return err
		}
		saved, err = readPreparation(ctx, tx, session.GetId())
		if err != nil {
			return err
		}
		receipt = preparationReceipt(saved, actorUID)
		return nil
	})
	return receipt, err
}

// GetWorkspacePreparationReceipt returns the Session's receipt, or nil when no
// preparation was requested. Callers authorize access.
func (c *Client) GetWorkspacePreparationReceipt(ctx context.Context, sessionID string) (*apiv1.WorkspacePreparationReceipt, error) {
	record, err := readPreparation(ctx, c.db, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	actorUID, err := sessionActorUID(ctx, c.db, sessionID)
	if err != nil {
		return nil, err
	}
	return preparationReceipt(record, actorUID), nil
}

// RuntimePreparation is what the runtime reads on GetWorkspace.
type RuntimePreparation struct {
	Required   bool
	Ready      bool
	Assignment *runtimev1.WorkspacePreparation
}

// IssueWorkspacePreparation reports the runtime's preparation state and issues
// a pending assignment to actorUID, the authenticated Actor. The first read
// records the issue; later reads return the same assignment until it settles.
// An assignment for another Actor is never issued.
//
// The runtime calls this before every turn, so it takes no lock: a recorded
// preparation is itself proof that one is required, and only a Session without
// one decodes its revision bundle. A preparation recorded concurrently is
// issued on the runtime's next read.
func (c *Client) IssueWorkspacePreparation(ctx context.Context, session *apiv1.Session, actorUID string) (*RuntimePreparation, error) {
	record, err := readPreparation(ctx, c.db, session.GetId())
	if errors.Is(err, pgx.ErrNoRows) {
		required, err := sessionRequiresPreparation(ctx, c.db, session)
		return &RuntimePreparation{Required: required}, err
	}
	if err != nil {
		return nil, err
	}
	state := &RuntimePreparation{Required: true, Ready: record.row.Phase == PreparationConfirmed}
	if record.row.Phase != PreparationPending || record.assignment.GetActorUid() != actorUID {
		return state, nil
	}
	state.Assignment = record.assignment
	if record.row.IssuedAt == nil {
		err = execSQL(ctx, c.db, `UPDATE session_workspace_preparation SET issued_at = now() WHERE session_id = $1 AND issued_at IS NULL`, session.GetId())
	}
	return state, err
}

// CompleteWorkspacePreparation settles an issued assignment with the runtime's
// report. Only actorUID's exact assignment is accepted. Repeating the committed
// report is a no-op; a different report for a settled preparation is a conflict.
func (c *Client) CompleteWorkspacePreparation(ctx context.Context, actorUID string, result *runtimev1.TaskStoreServiceCompleteWorkspacePreparationRequest) error {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(result)
	if err != nil {
		return err
	}
	return c.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, result.GetSessionId()); err != nil {
			return notFoundOr(err)
		}
		record, err := readPreparation(ctx, tx, result.GetSessionId())
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFailedPrecondition
		}
		if err != nil {
			return err
		}
		if !proto.Equal(record.assignment, result.GetAssignment()) || record.assignment.GetActorUid() != actorUID || record.row.IssuedAt == nil {
			return ErrFailedPrecondition
		}
		if record.row.Phase != PreparationPending {
			if proto.Equal(record.result, result) {
				return nil
			}
			return ErrConflict
		}
		phase, observed := PreparationDefiniteFailure, (*time.Time)(nil)
		if result.GetConfirmed() {
			at := result.GetObservedAt().AsTime()
			phase, observed = PreparationConfirmed, &at
		}
		return execSQL(ctx, tx, `
			UPDATE session_workspace_preparation SET phase = $2, result_data = $3, effect_observed_at = $4, settled_at = now()
			WHERE session_id = $1
		`, result.GetSessionId(), phase, data, observed)
	})
}

// requirePreparedAdmission refuses a new task until a required preparation is
// confirmed. It runs under the Session lock.
func requirePreparedAdmission(ctx context.Context, tx pgx.Tx, session *apiv1.Session) error {
	required, err := sessionRequiresPreparation(ctx, tx, session)
	if err != nil || !required {
		return err
	}
	record, err := readPreparation(ctx, tx, session.GetId())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && record.row.Phase != PreparationConfirmed) {
		return fmt.Errorf("workspace preparation is not confirmed: %w", ErrFailedPrecondition)
	}
	return err
}
