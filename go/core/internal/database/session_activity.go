package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

const (
	// RuntimeActivityLease is how long an active report holds idle suspension.
	RuntimeActivityLease = time.Minute
	// RuntimeActivityRenewAfter is how often an active runtime reports again.
	// Two renewals fit in one lease, so one lost report does not end it.
	RuntimeActivityRenewAfter = 20 * time.Second
)

// RecordRuntimeActivity records a runtime's activity report. The caller has
// authenticated the runtime as the Session's current Actor.
//
// An active report holds idle suspension, including task-boundary suspension,
// until the lease ends; an idle report ends the lease now. Either way the
// runtime becomes eligible for idle suspension quiesceDelay after the lease
// ends. A claim already held keeps its fence; a report after it leaves the
// boundary pending for the next claim (see FinishSessionActivityQuiescence).
func (c *Client) RecordRuntimeActivity(ctx context.Context, sessionID string, active bool, lease, quiesceDelay time.Duration) error {
	if !active {
		lease = 0
	}
	return c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return notFoundOr(err)
		}
		switch row.State {
		case apiv1.RuntimeState_RUNTIME_STATE_DELETING.String(), apiv1.RuntimeState_RUNTIME_STATE_DELETED.String():
			return fmt.Errorf("session is being deleted: %w", ErrFailedPrecondition)
		}
		return execSQL(ctx, tx, `
			INSERT INTO session_activity (session_id, active_until, idle_due_at, reported_at, pending, report_seq)
			VALUES ($1, clock_timestamp() + $2::bigint * INTERVAL '1 microsecond',
			    clock_timestamp() + ($2::bigint + $3::bigint) * INTERVAL '1 microsecond',
			    clock_timestamp(), TRUE, 1)
			ON CONFLICT (session_id) DO UPDATE SET
			    active_until = EXCLUDED.active_until, idle_due_at = EXCLUDED.idle_due_at,
			    reported_at = EXCLUDED.reported_at, pending = TRUE,
			    report_seq = session_activity.report_seq + 1
		`, row.ID, max(lease, 0).Microseconds(), boundedQuiesceDelay(quiesceDelay).Microseconds())
	})
}

// SessionActivityQuiescence grants one worker permission to suspend a runtime
// whose reported activity has ended.
type SessionActivityQuiescence struct {
	Session    *apiv1.Session
	ReportSeq  int64
	ExecutorID uuid.UUID
}

// ClaimSessionActivityQuiescence claims one idle activity boundary. It requires
// a settled READY runtime with no pending task boundary, whose own claim records
// the task snapshot. The claim fences new execution like a task-boundary claim.
// It never expires: a Substrate call with an unknown outcome may still complete,
// so the fence stays until the claim finishes or crash recovery or deletion
// retires idle work. Missing work returns ErrNotFound.
func (c *Client) ClaimSessionActivityQuiescence(ctx context.Context) (*SessionActivityQuiescence, error) {
	var result *SessionActivityQuiescence
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		candidate, err := queryOne(ctx, tx, `
			SELECT i.id::text FROM session_activity a JOIN session_record i ON i.id = a.session_id
			WHERE a.pending AND a.executor_id IS NULL AND a.idle_due_at <= statement_timestamp()
			  AND i.state = 'RUNTIME_STATE_READY' AND i.operation = 'RUNTIME_OPERATION_NONE'
			ORDER BY a.idle_due_at LIMIT 1 FOR UPDATE OF i SKIP LOCKED
		`, pgx.RowTo[string])
		if err != nil {
			return notFoundOr(err)
		}
		row, err := readSession(ctx, tx, candidate)
		if err != nil {
			return notFoundOr(err)
		}
		if row.State != "RUNTIME_STATE_READY" || row.Operation != "RUNTIME_OPERATION_NONE" {
			return ErrNotFound
		}
		executorID := uuid.New()
		// Recheck everything after locking: the candidate query's snapshot may
		// predate a dispatch, task or report committed just before the lock.
		seq, err := queryOne(ctx, tx, `
			UPDATE session_activity a SET executor_id = $2, claimed_seq = a.report_seq
			FROM session s
			WHERE a.session_id = $1 AND s.id = a.session_id AND s.history_id = $3
			  AND a.pending AND a.executor_id IS NULL AND a.idle_due_at <= clock_timestamp()
			  AND (s.dispatch_expires_at IS NULL OR s.dispatch_expires_at <= clock_timestamp())
			  AND NOT EXISTS (SELECT 1 FROM session_task_event e WHERE e.history_id = $3
			      AND (NOT e.published OR e.quiescence_pending))
			  AND NOT EXISTS (SELECT 1 FROM session_task t WHERE t.history_id = $3
			      AND t.state IN ('TASK_STATE_SUBMITTED', 'TASK_STATE_WORKING'))
			  AND NOT EXISTS (SELECT 1 FROM session_checkpoint WHERE source_session_id = $1 AND state = 'CREATING')
			RETURNING a.report_seq
		`, pgx.RowTo[int64], row.ID, executorID, row.HistoryID)
		if err != nil {
			return notFoundOr(err)
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		result = &SessionActivityQuiescence{Session: session, ReportSeq: seq, ExecutorID: executorID}
		return nil
	})
	return result, err
}

// FinishSessionActivityQuiescence records a claimed outcome and releases the
// fence. The boundary closes only when no report arrived after the claim; a
// later report proves the runtime ran again and leaves it pending. A stale or
// retired claim returns ErrNotFound and changes nothing.
func (c *Client) FinishSessionActivityQuiescence(ctx context.Context, work *SessionActivityQuiescence) error {
	if work == nil || work.Session == nil || work.ExecutorID == uuid.Nil {
		return fmt.Errorf("claimed activity boundary is required")
	}
	return c.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, work.Session.GetId()); err != nil {
			return notFoundOr(err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE session_activity SET pending = report_seq <> claimed_seq,
			    executor_id = NULL, claimed_seq = NULL
			WHERE session_id = $1 AND executor_id = $2
		`, work.Session.GetId(), work.ExecutorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// retireSessionActivity drops activity boundaries and claims for a runtime
// that no longer needs idle suspension. The caller holds the Session lock.
func retireSessionActivity(ctx context.Context, tx pgx.Tx, session sessionRow) error {
	return execSQL(ctx, tx, `
		UPDATE session_activity SET pending = FALSE, executor_id = NULL, claimed_seq = NULL
		WHERE session_id = $1
	`, session.ID)
}
