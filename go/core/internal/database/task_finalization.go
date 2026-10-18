package database

import (
	"context"
	"errors"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// SettleSessionTask publishes a saved terminal/waiting task and its history
// atomically after native cleanup. It does not wait for runtime pause or snapshots.
// The bounded delay starts at publication and is durable; retries keep its deadline.
// Retries cannot republish an old state over a later turn. Callers authenticate
// runtime authority; a missing or deleted session returns ErrNotFound.
func (c *Client) SettleSessionTask(ctx context.Context, sessionID, taskID string, version int64, quiesceDelay time.Duration) error {
	return c.withTx(ctx, func(tx pgx.Tx) error {
		session, err := lockSession(ctx, tx, sessionID)
		if err != nil {
			return notFoundOr(err)
		}
		if session.State == "RUNTIME_STATE_DELETED" {
			return ErrNotFound
		}
		type boundary struct {
			Data            []byte
			ExpectedVersion int64
			Published       bool
		}
		row, err := queryOne(ctx, tx, `
			SELECT data, expected_version, published FROM session_task_event
			WHERE history_id = $1 AND task_id = $2 AND sequence = $3
			  AND expected_version IS NOT NULL AND quiescence_pending IS NOT NULL
		`, pgx.RowToStructByName[boundary], session.HistoryID, taskID, version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if row.Published {
			return nil
		}
		stored, err := readSessionTask(ctx, tx, session.HistoryID, taskID)
		if err != nil {
			return err
		}
		wire, err := applyStoredBoundary(stored.Data, row.Data)
		if err != nil {
			return err
		}
		task, err := pbconv.FromProtoTask(wire)
		if err != nil {
			return err
		}
		data, err := proto.Marshal(wire)
		if err != nil {
			return err
		}
		if _, err := saveTaskProjection(ctx, tx, session.HistoryID, taskID, string(task.Status.State), task.Status.Timestamp, data); err != nil {
			return err
		}
		return execSQL(ctx, tx, `
			UPDATE session_task_event SET published = TRUE,
			    quiescence_due_at = CASE WHEN sequence = $4
			        THEN clock_timestamp() + $5::bigint * INTERVAL '1 microsecond'
			        ELSE quiescence_due_at END
			WHERE history_id = $1 AND task_id = $2 AND sequence > $3 AND sequence <= $4
		`, session.HistoryID, taskID, row.ExpectedVersion, version, boundedQuiesceDelay(quiesceDelay).Microseconds())
	})
}

// AbandonedBoundaryAfter is how long a saved boundary may wait for its native
// cleanup callback before the control plane settles it for the runtime. Cleanup
// takes seconds; a boundary this old belongs to a runtime that died or gave up.
const AbandonedBoundaryAfter = 10 * time.Minute

// SettleAbandonedSessionBoundaries publishes boundaries whose runtime never
// called SettleSessionTask within olderThan, so a dead runtime cannot hold
// lifecycle admission closed forever. It settles with no quiesce delay through
// SettleSessionTask, which rechecks under the session lock; a runtime that
// settles concurrently wins harmlessly. Publishing keeps the history the
// runtime already saved, and the idle worker then suspends the Actor as for any
// settled turn. It returns the number of boundaries it settled, and stops at
// the first error.
func (c *Client) SettleAbandonedSessionBoundaries(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	type abandoned struct {
		SessionID string
		TaskID    string
		Sequence  int64
	}
	candidates, err := queryMany(ctx, c.db, `
		SELECT i.id::text AS session_id, e.task_id, e.sequence
		FROM session_task_event e JOIN session_record i ON i.history_id = e.history_id
		WHERE NOT e.published AND e.expected_version IS NOT NULL AND e.quiescence_pending IS NOT NULL
		  AND e.created_at <= clock_timestamp() - $1::bigint * INTERVAL '1 microsecond'
		  AND i.state <> 'RUNTIME_STATE_DELETED'
		ORDER BY e.sequence LIMIT $2
	`, pgx.RowToStructByName[abandoned], olderThan.Microseconds(), limit)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, candidate := range candidates {
		if err := c.SettleSessionTask(ctx, candidate.SessionID, candidate.TaskID, candidate.Sequence, 0); err != nil {
			// A deleted session or a superseded boundary has nothing left to settle.
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
				continue
			}
			return settled, err
		}
		settled++
	}
	return settled, nil
}

// boundedQuiesceDelay keeps post-turn execution within zero to thirty minutes.
func boundedQuiesceDelay(delay time.Duration) time.Duration {
	return min(max(delay, 0), 30*time.Minute)
}

func applyStoredBoundary(data, eventData []byte) (*a2apb.Task, error) {
	task, event := &a2apb.Task{}, &a2apb.StreamResponse{}
	if err := proto.Unmarshal(data, task); err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(eventData, event); err != nil {
		return nil, err
	}
	return applyTaskEvent(task, event)
}
