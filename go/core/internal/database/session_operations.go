package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionOperation is the current lifecycle generation on a session. Once
// Session.Operation is unspecified, callers may return the current session.
// ExecutorID marks possibly issued runtime work and never expires. The value is
// an observation, not permission to execute: callers must win Claim first.
type SessionOperation struct {
	RuntimeOperation[*apiv1alpha1.Session]
	SourceCheckpointID *uuid.UUID
}

// BeginSessionOperation admits or joins current lifecycle work under the
// session lock. Creation retries and already-at-target requests return current
// state. Delete alone may supersede unclaimed work; uncertain work blocks it.
// Callers authorize access. Deleted sessions return ErrNotFound, except Delete
// can observe the tombstone for a caller already authorized before deletion.
func (c *Client) BeginSessionOperation(ctx context.Context, sessionID string, kind apiv1alpha1.RuntimeOperation) (*SessionOperation, error) {
	var operation *SessionOperation
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return notFoundOr(err)
		}
		if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
			operation, err = beginSessionDeletion(ctx, tx, row, sessionDeletionUserRequested)
			return err
		}
		operation, err = beginSessionOperation(ctx, tx, row, kind)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("begin Session operation: %w", err)
	}
	return operation, nil
}

// beginSessionOperation admits or joins a lifecycle transition in the caller's
// transaction. The caller holds the session lock and owns any additional policy.
func beginSessionOperation(ctx context.Context, tx pgx.Tx, row sessionRow, kind apiv1alpha1.RuntimeOperation) (*SessionOperation, error) {
	needed, err := runtimeOperationNeeded(row.runtimeInstanceRow, kind)
	if err != nil {
		return nil, err
	}
	if needed {
		if err := admitAgentLifecycle(ctx, tx, row, kind); err != nil {
			return nil, err
		}
		if kind == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE {
			// Deletion keeps task admission closed even if preparation fails
			// and releases this operation. Only a delete retry can finish it.
			row.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING.String()
		}
		row.runtimeInstanceRow, err = beginRuntimeOperation(ctx, tx, row.runtimeInstanceRow, runtimeKindAgent, kind)
		if err != nil {
			return nil, err
		}
	}
	operation, err := toSessionOperation(row)
	if err != nil {
		return nil, err
	}
	if needed {
		operation.Instance.UpdatedAt = timestamppb.Now()
		data, err := marshalSession(operation.Instance)
		if err != nil {
			return nil, err
		}
		if err := execSQL(ctx, tx, `UPDATE session SET data = $2 WHERE id = $1`, row.ID, data); err != nil {
			return nil, err
		}
	}
	return operation, nil
}

// admitAgentLifecycle runs under the same row lock as task and checkpoint
// admission, including the exact assigned native preparation. Common transitions have no knowledge of these agent-only records.
func admitAgentLifecycle(ctx context.Context, tx pgx.Tx, row sessionRow, kind apiv1alpha1.RuntimeOperation) error {
	if err := requireSettledRuntime(ctx, tx, row.HistoryID, ""); err != nil {
		return err
	}
	busy, err := queryOne(ctx, tx, `
  SELECT EXISTS (SELECT 1 FROM session_checkpoint WHERE source_session_id = $1 AND state = 'CREATING')
   OR ($2::text = 'RUNTIME_OPERATION_SUSPEND' AND EXISTS (
    SELECT 1 FROM session_task WHERE history_id = $3
     AND state NOT IN ('TASK_STATE_COMPLETED', 'TASK_STATE_CANCELED', 'TASK_STATE_FAILED',
      'TASK_STATE_REJECTED', 'TASK_STATE_INPUT_REQUIRED', 'TASK_STATE_AUTH_REQUIRED')))
 `, pgx.RowTo[bool], row.ID, kind.String(), row.HistoryID)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("session has an active task or checkpoint: %w", ErrConflict)
	}
	return nil
}

// ClaimSessionOperation grants one bounded attempt permission to issue this
// generation's runtime work. An ended or expired attempt can be retried, but a
// stale generation cannot claim, even when later work has the same kind and state.
func (c *Client) ClaimSessionOperation(ctx context.Context, sessionID string, id, executorID uuid.UUID) (bool, error) {
	return c.claimRuntimeOperation(ctx, sessionID, runtimeKindAgent, id, executorID)
}

// FinishObservedSessionSuspension settles a verified already-suspended Actor
// without granting permission to issue runtime work. The caller observes the
// Actor outside this transaction. Under the Session then generation locks, the
// operation must still be the same never-claimed READY/SUSPEND, with the same
// prepared revision and active Actor UID. A concurrent claim or supersession
// returns ErrConflict; callers reconcile completed work through Begin/Get.
func (c *Client) FinishObservedSessionSuspension(ctx context.Context, sessionID string, id uuid.UUID, preparedRevision, actorUID string) (*apiv1alpha1.Session, error) {
	var result *apiv1alpha1.Session
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if id == uuid.Nil || row.OperationID == nil || *row.OperationID != id || row.ExecutorID != nil ||
			row.State != apiv1alpha1.RuntimeState_RUNTIME_STATE_READY.String() ||
			row.Operation != apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_SUSPEND.String() ||
			preparedRevision == "" || derefStr(row.PreparedRevision) != preparedRevision || actorUID == "" {
			return ErrConflict
		}
		uid, err := queryOne(ctx, tx, `
			SELECT actor_uid FROM runtime_generation WHERE session_id = $1 AND phase = 'active' FOR UPDATE
		`, pgx.RowTo[string], row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if uid != actorUID {
			return ErrConflict
		}
		next := row.runtimeInstanceRow
		next.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED.String()
		next.Operation = apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE.String()
		result, err = persistSessionOperationResult(ctx, tx, row, next, "", actorUID, "")
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("finish observed Session suspension: %w", err)
	}
	return result, nil
}

// FinishSessionOperation publishes known success only for the claiming
// executor. A nonempty failure releases only unclaimed preparation and invalidates
// its generation. Uncertain issued work must remain pending. Stale completion or
// release returns ErrConflict. Deletion revokes shares and releases resource
// pins atomically. Explicit deletion retains a tombstone; idle deletion also
// removes the session and creation receipt in this transaction.
func (c *Client) FinishSessionOperation(ctx context.Context, sessionID string, id, executorID uuid.UUID, authority, actorUID, failure string) (*apiv1alpha1.Session, error) {
	var result *apiv1alpha1.Session
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		next, err := completeRuntimeOperation(row.runtimeInstanceRow, id, executorID, failure)
		if err != nil {
			return err
		}
		result, err = persistSessionOperationResult(ctx, tx, row, next, authority, actorUID, failure)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("finish Session operation: %w", err)
	}
	return result, nil
}

// persistSessionOperationResult publishes lifecycle and Session data together.
// The caller holds the Session lock and has verified completion authority.
func persistSessionOperationResult(ctx context.Context, tx pgx.Tx, row sessionRow, next runtimeInstanceRow, authority, actorUID, failure string) (*apiv1alpha1.Session, error) {
	result, err := toSession(row)
	if err != nil {
		return nil, err
	}
	if failure == "" {
		switch result.Operation {
		case apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE:
			if authority == "" || actorUID == "" {
				return nil, fmt.Errorf("created Session requires runtime authority and actor UID")
			}
			result.A2AAuthority = authority
		case apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_DELETE:
			result.A2AAuthority = ""
			if err := releaseAgentRuntimeReferences(ctx, tx, row.ID); err != nil {
				return nil, err
			}
		}
	}
	result.State, result.Operation, err = next.lifecycle()
	if err != nil {
		return nil, err
	}
	result.PreparedRevision = derefStr(next.PreparedRevision)
	result.UpdatedAt = timestamppb.Now()
	result.Failure = nil
	if failure != "" {
		result.Failure = &apiv1alpha1.Failure{Reason: "PreparationFailed", Message: failure}
	}
	row.Data, err = marshalSession(result)
	if err != nil {
		return nil, err
	}
	if err := saveRuntimeLifecycle(ctx, tx, next, runtimeKindAgent); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `
        UPDATE session SET data = $2,
            actor_uid = CASE WHEN $4 THEN $3 ELSE actor_uid END
        WHERE id = $1 AND ($3::text = '' OR actor_uid = $3 OR (actor_uid IS NULL AND $4))
    `, row.ID, row.Data, actorUID, failure == "" && row.Operation == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_CREATE.String())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("runtime actor UID changed: %w", ErrConflict)
	}
	row.runtimeInstanceRow = next
	result, err = toSession(row)
	if err != nil {
		return nil, err
	}
	if result.State == apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED && failure == "" {
		if err := finishSessionDeletion(ctx, tx, row.ID); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// GetSessionOperation observes this generation only while it remains current,
// including its deletion tombstone. A superseded or missing generation returns
// ErrConflict. Callers must have authorized the session at admission. It never
// returns historical results or grants permission to issue runtime work.
func (c *Client) GetSessionOperation(ctx context.Context, sessionID string, id uuid.UUID) (*SessionOperation, error) {
	row, err := queryOne(ctx, c.db, `
		SELECT id, user_id, prepared_revision, state, data, operation, context_id,
		    source_checkpoint_id, history_id, operation_id, executor_id FROM session_record
		WHERE id = $1 AND operation_id = $2
	`, pgx.RowToStructByName[sessionRow], sessionID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("lifecycle generation was superseded: %w", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	return toSessionOperation(row)
}

// toSessionOperation decodes current session state and its private execution
// identity. A completed generation observes current fields, including renames.
func toSessionOperation(row sessionRow) (*SessionOperation, error) {
	session, err := toSession(row)
	if err != nil {
		return nil, err
	}
	return &SessionOperation{RuntimeOperation: runtimeOperationFor(row.runtimeInstanceRow, session), SourceCheckpointID: row.SourceCheckpointID}, nil
}
