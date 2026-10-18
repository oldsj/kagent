package database

import (
	"context"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// SessionQuiescence grants one worker permission to pause an idle session or
// suspend it and record its snapshot. Task publication has already completed.
type SessionQuiescence struct {
	Session    *apiv1alpha1.Session
	TaskID     string
	State      a2a.TaskState
	Version    int64
	ExecutorID uuid.UUID
}

// DefaultQuiescenceClaimLease bounds a claim when the caller names no lease.
const DefaultQuiescenceClaimLease = DefaultRuntimeOperationTimeout

// ClaimSessionQuiescence claims an idle session at its latest settled task
// version. A new turn supersedes unclaimed idle work; a claim blocks task writes,
// checkpoints, and explicit lifecycle operations until it finishes. Missing work,
// including boundaries before their durable deadline, returns ErrNotFound.
// Older writers leave a NULL deadline, which keeps immediate eligibility.
//
// A claim moves the event's deadline to now+lease, so one predicate covers first
// claims and takeovers: once the lease ends, or the claim has no deadline (a
// crashed older writer left it), the next worker claims the same boundary under
// a new executor ID. The lease must outlast one attempt, because the previous
// executor may still be inside its Substrate call; Quiesce observes an Actor that
// is already suspended instead of suspending it again. The superseded executor
// cannot finish, since its ID no longer matches. A zero or negative lease uses
// DefaultQuiescenceClaimLease.
func (c *Client) ClaimSessionQuiescence(ctx context.Context, lease time.Duration) (*SessionQuiescence, error) {
	if lease <= 0 {
		lease = DefaultQuiescenceClaimLease
	}
	var result *SessionQuiescence
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		type candidate struct {
			SessionID string
			TaskID    string
			Sequence  int64
		}
		row, err := queryOne(ctx, tx, `
			SELECT i.id::text AS session_id, e.task_id, e.sequence
			FROM session_task_event e JOIN session_record i ON i.history_id = e.history_id
			WHERE e.published AND e.quiescence_pending = TRUE
			  AND (e.quiescence_due_at IS NULL OR e.quiescence_due_at <= statement_timestamp())
			  AND i.state = 'RUNTIME_STATE_READY'
			  AND i.operation = 'RUNTIME_OPERATION_NONE'
			  AND (i.dispatch_expires_at IS NULL OR i.dispatch_expires_at <= clock_timestamp())
			  AND NOT EXISTS (SELECT 1 FROM session_checkpoint WHERE source_session_id = i.id AND state = 'CREATING')
			ORDER BY e.sequence LIMIT 1 FOR UPDATE OF i SKIP LOCKED
		`, pgx.RowToStructByName[candidate])
		if err != nil {
			return notFoundOr(err)
		}
		session, err := readSession(ctx, tx, row.SessionID)
		if err != nil {
			return err
		}
		if session.State != "RUNTIME_STATE_READY" || session.Operation != "RUNTIME_OPERATION_NONE" {
			return ErrNotFound
		}
		stored, err := readSessionTask(ctx, tx, session.HistoryID, row.TaskID)
		if err != nil {
			return err
		}
		task, err := unmarshalSessionTask(stored.Data)
		if err != nil {
			return err
		}
		value, err := toSession(session)
		if err != nil {
			return err
		}
		result = &SessionQuiescence{Session: value, TaskID: row.TaskID, State: task.Status.State, Version: row.Sequence, ExecutorID: uuid.New()}
		// Recheck after locking: the candidate query's snapshot may predate a
		// new turn that committed just before we acquired the session lock.
		tag, err := tx.Exec(ctx, `
			UPDATE session_task_event SET quiescence_executor_id = $2,
			    quiescence_due_at = clock_timestamp() + $4::bigint * INTERVAL '1 microsecond'
			WHERE sequence = $1 AND published AND quiescence_pending
			  AND (quiescence_due_at IS NULL OR quiescence_due_at <= statement_timestamp())
			  AND NOT EXISTS (SELECT 1 FROM session WHERE id = $3 AND dispatch_expires_at > clock_timestamp())
			  AND NOT EXISTS (SELECT 1 FROM session_checkpoint WHERE source_session_id = $3 AND state = 'CREATING')
		`, result.Version, result.ExecutorID, session.ID, lease.Microseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
	return result, err
}

// FinishSessionQuiescence records a claimed pause/suspend outcome and releases
// the session for new execution. Terminal tasks require the matching snapshot.
// Retries are harmless, and a stale claim cannot release another operation.
// Task state and history remain readable throughout lifecycle work.
func (c *Client) FinishSessionQuiescence(ctx context.Context, work *SessionQuiescence, snapshot *SessionTaskSnapshot) error {
	if work != nil && work.State.Terminal() && (snapshot == nil || snapshot.URI == "" || snapshot.Atespace == "" || snapshot.ContentScope == "") {
		return fmt.Errorf("terminal task quiescence requires a runtime snapshot")
	}
	return c.finishSessionQuiescence(ctx, work, snapshot)
}

// FinishLostSessionQuiescence settles a claim whose runtime crashed, so no
// checkpoint exists. The task keeps no snapshot and cannot be checkpointed or
// forked; the session is released as for any other settled boundary.
func (c *Client) FinishLostSessionQuiescence(ctx context.Context, work *SessionQuiescence) error {
	return c.finishSessionQuiescence(ctx, work, nil)
}

func (c *Client) finishSessionQuiescence(ctx context.Context, work *SessionQuiescence, snapshot *SessionTaskSnapshot) error {
	if work == nil || work.ExecutorID == uuid.Nil {
		return fmt.Errorf("claimed task boundary is required")
	}
	return c.withTx(ctx, func(tx pgx.Tx) error {
		session, err := lockSession(ctx, tx, work.Session.Id)
		if err != nil {
			return notFoundOr(err)
		}
		if session.State == "RUNTIME_STATE_DELETED" {
			return ErrNotFound
		}
		pending, err := queryOne(ctx, tx, `
			SELECT quiescence_pending FROM session_task_event
			WHERE sequence = $1 AND history_id = $2 AND task_id = $3 AND quiescence_executor_id = $4
		`, pgx.RowTo[bool], work.Version, session.HistoryID, work.TaskID, work.ExecutorID)
		if err != nil {
			return notFoundOr(err)
		}
		if !pending {
			return nil
		}
		stored, err := readSessionTask(ctx, tx, session.HistoryID, work.TaskID)
		if err != nil {
			return err
		}
		if stored.State != string(work.State) {
			return ErrConflict
		}
		if snapshot != nil {
			if err := execSQL(ctx, tx, `
				UPDATE session_task SET snapshot_atespace = $3, snapshot_uri = $4,
				    snapshot_content_scope = $5, history_sequence = $6
				WHERE history_id = $1 AND id = $2
			`, session.HistoryID, work.TaskID, snapshot.Atespace, snapshot.URI, snapshot.ContentScope, work.Version); err != nil {
				return err
			}
			if err := execSQL(ctx, tx, `
				UPDATE session_task_event SET snapshot_atespace = $2, snapshot_uri = $3, snapshot_content_scope = $4
				WHERE sequence = $1
			`, work.Version, snapshot.Atespace, snapshot.URI, snapshot.ContentScope); err != nil {
				return err
			}
		}
		return execSQL(ctx, tx, `
			UPDATE session_task_event SET quiescence_pending = FALSE WHERE sequence = $1
		`, work.Version)
	})
}
