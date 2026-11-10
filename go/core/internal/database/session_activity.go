package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// RuntimeActivityLease is how long an active report holds idle quiescence.
	RuntimeActivityLease = time.Minute
	// RuntimeActivityRenewAfter is how often an active runtime reports again.
	// Two renewals fit in one lease, so one lost report does not end it.
	RuntimeActivityRenewAfter = 20 * time.Second
)

// RecordRuntimeActivity records a runtime's activity report and opens an idle
// boundary for it. The caller has already observed the Actor running under the
// Session's active runtime generation. A report for an explicitly SUSPENDED
// Session with no pending operation adopts the wake that Substrate ingress
// performed without the control plane: the Session becomes READY.
//
// An active report holds idle quiescence, including task-boundary quiescence,
// until lease ends; an idle report ends the lease now. Either way the boundary
// becomes claimable quiesceDelay after the lease ends. A claim already held
// keeps its fence; a report after it keeps the boundary pending for the next
// claim (see FinishSessionActivityQuiescence).
func (c *Client) RecordRuntimeActivity(ctx context.Context, sessionID string, active bool, lease, quiesceDelay time.Duration) error {
	if !active {
		lease = 0
	}
	return c.withTx(ctx, func(tx pgx.Tx) error {
		row, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return notFoundOr(err)
		}
		state, operation, err := row.lifecycle()
		if err != nil {
			return err
		}
		switch state {
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETING, apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED:
			return fmt.Errorf("session is being deleted: %w", ErrFailedPrecondition)
		case apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED:
			if operation == apiv1alpha1.RuntimeOperation_RUNTIME_OPERATION_NONE {
				if err := adoptRuntimeWake(ctx, tx, row); err != nil {
					return err
				}
			}
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
		`, row.ID, lease.Microseconds(), max(quiesceDelay, 0).Microseconds())
	})
}

// adoptRuntimeWake moves an explicitly suspended Session whose Actor is
// running to READY. Suspension keeps the runtime generation, credentials and
// A2A authority, so no lifecycle work remains; the caller holds the lock.
func adoptRuntimeWake(ctx context.Context, tx pgx.Tx, row sessionRow) error {
	session, err := toSession(row)
	if err != nil {
		return err
	}
	session.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_READY
	session.UpdatedAt = timestamppb.Now()
	data, err := marshalSession(session)
	if err != nil {
		return err
	}
	next := row.runtimeInstanceRow
	next.State = apiv1alpha1.RuntimeState_RUNTIME_STATE_READY.String()
	if err := saveRuntimeLifecycle(ctx, tx, next, runtimeKindAgent); err != nil {
		return err
	}
	return execSQL(ctx, tx, `UPDATE session SET data = $2 WHERE id = $1`, row.ID, data)
}

// SessionActivityQuiescence grants one worker permission to pause or suspend a
// runtime whose reported activity has ended. Waiting reports a task parked in
// INPUT_REQUIRED or AUTH_REQUIRED, whose native process a pause must keep.
type SessionActivityQuiescence struct {
	Session    *apiv1alpha1.Session
	Waiting    bool
	ReportSeq  int64
	ExecutorID uuid.UUID
}

// ClaimSessionActivityQuiescence claims one idle activity boundary. It requires
// the same settled runtime as task-boundary quiescence and, in addition, no
// pending task boundary: that boundary's own claim records its snapshot. The
// claim fences new execution like a task-boundary claim until it finishes or
// its lease ends; the next worker then takes it over under a new executor ID.
func (c *Client) ClaimSessionActivityQuiescence(ctx context.Context, lease time.Duration) (*SessionActivityQuiescence, error) {
	if lease <= 0 {
		lease = DefaultQuiescenceClaimLease
	}
	var result *SessionActivityQuiescence
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		candidate, err := queryOne(ctx, tx, `
			SELECT i.id::text FROM session_activity a JOIN session_record i ON i.id = a.session_id
			WHERE a.pending AND a.idle_due_at <= statement_timestamp()
			  AND (a.executor_id IS NULL OR a.claim_expires_at <= statement_timestamp())
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
		type claim struct {
			ReportSeq int64
			Waiting   bool
		}
		executorID := uuid.New()
		// Recheck everything after locking: the candidate query's snapshot may
		// predate a dispatch, task or report committed just before the lock.
		claimed, err := queryOne(ctx, tx, `
			UPDATE session_activity a SET executor_id = $2, claimed_seq = a.report_seq,
			    claim_expires_at = clock_timestamp() + $4::bigint * INTERVAL '1 microsecond'
			FROM session s
			WHERE a.session_id = $1 AND s.id = a.session_id AND s.history_id = $3
			  AND a.pending AND a.idle_due_at <= clock_timestamp()
			  AND (a.executor_id IS NULL OR a.claim_expires_at <= clock_timestamp())
			  AND (s.dispatch_expires_at IS NULL OR s.dispatch_expires_at <= clock_timestamp())
			  AND NOT EXISTS (SELECT 1 FROM session_task_event e WHERE e.history_id = $3
			      AND (NOT e.published OR e.quiescence_pending))
			  AND NOT EXISTS (SELECT 1 FROM session_task t WHERE t.history_id = $3
			      AND t.state IN ('TASK_STATE_SUBMITTED', 'TASK_STATE_WORKING'))
			  AND NOT EXISTS (SELECT 1 FROM session_checkpoint WHERE source_session_id = $1 AND state = 'CREATING')
			  AND NOT EXISTS (SELECT 1 FROM session_native_preparation n WHERE n.session_id = $1
			      AND n.phase IN ('pending', 'uncertain'))
			RETURNING a.report_seq,
			    EXISTS (SELECT 1 FROM session_task t WHERE t.history_id = $3
			        AND t.state IN ('TASK_STATE_INPUT_REQUIRED', 'TASK_STATE_AUTH_REQUIRED')) AS waiting
		`, pgx.RowToStructByName[claim], row.ID, executorID, row.HistoryID, lease.Microseconds())
		if err != nil {
			return notFoundOr(err)
		}
		session, err := toSession(row)
		if err != nil {
			return err
		}
		result = &SessionActivityQuiescence{Session: session, Waiting: claimed.Waiting, ReportSeq: claimed.ReportSeq, ExecutorID: executorID}
		return nil
	})
	return result, err
}

// FinishSessionActivityQuiescence records a claimed outcome and releases the
// fence. The boundary closes only when no report arrived after the claim; a
// later report proves the runtime ran again and leaves it pending. A stale
// executor returns ErrNotFound and changes nothing.
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
			    executor_id = NULL, claim_expires_at = NULL, claimed_seq = NULL
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
