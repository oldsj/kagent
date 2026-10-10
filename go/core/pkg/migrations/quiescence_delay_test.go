package migrations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestQuiescenceDelayMigrationPreservesLegacyRowsAndRollsBack(t *testing.T) {
	dsn := startTestDB(t)
	source := BuiltinSources(false)[0]
	ctx := t.Context()
	require.NoError(t, WithProvider(ctx, dsn, source, func(provider *goose.Provider) error {
		_, err := provider.UpTo(ctx, 6)
		return err
	}))
	historyID := "00000000-0000-0000-0000-000000000001"
	execSQL(t, dsn, `INSERT INTO a2a_context (id, context_id) VALUES ($1, $1)`, historyID)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	execSQL(t, dsn, `
		INSERT INTO session_task_event (history_id, task_id, data, created_at, published, quiescence_pending)
		VALUES ($1, 'pending', $2, $3, TRUE, TRUE),
		       ($1, 'finished', $2, $3, TRUE, FALSE),
		       ($1, 'ordinary', $2, $3, TRUE, NULL)
	`, historyID, []byte{}, created)
	require.NoError(t, RunUp(ctx, dsn, []Source{source}))
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()
	for _, taskID := range []string{"pending", "finished", "ordinary"} {
		var due sql.NullTime
		require.NoError(t, db.QueryRowContext(ctx, `SELECT quiescence_due_at FROM session_task_event WHERE task_id = $1`, taskID).Scan(&due))
		require.False(t, due.Valid, "existing rows keep an unset deadline")
	}
	require.NoError(t, WithProvider(ctx, dsn, source, func(provider *goose.Provider) error {
		_, err := provider.DownTo(ctx, 6)
		return err
	}))
	require.False(t, testColumnExists(t, dsn, "session_task_event", "quiescence_due_at"))
	require.NoError(t, RunUp(context.Background(), dsn, []Source{source}))
}
