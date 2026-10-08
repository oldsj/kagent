package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestLocalFixtureTargetValidation(t *testing.T) {
	good := "postgres://postgres:kagent@127.0.0.1:33130/kagent_fixture?sslmode=disable"
	_, err := localURL(good)
	require.NoError(t, err)
	for _, raw := range []string{"", strings.Replace(good, "127.0.0.1", "example.org", 1), strings.Replace(good, "127.0.0.1", "localhost", 1), strings.Replace(good, "kagent_fixture", "postgres", 1), good + "&options=--search_path=foreign", strings.Replace(good, ":33130", ":543", 1), strings.Replace(good, "postgres://", "postgresql://", 1)} {
		_, err := localURL(raw)
		require.Error(t, err)
	}
}

func TestLocalFixtureIsolationAndCapturedCleanup(t *testing.T) {
	raw := os.Getenv("KAGENT_TEST_LOCAL_POSTGRES_URL")
	if raw == "" {
		t.Skip("requires explicit local fixture opt-in")
	}
	first, cleanupFirst, err := Start(t.Context())
	require.NoError(t, err)
	t.Cleanup(cleanupFirst)
	second, cleanupSecond, err := Start(t.Context())
	require.NoError(t, err)
	t.Cleanup(cleanupSecond)
	require.NotEqual(t, first, second)
	one, err := pgx.Connect(t.Context(), first)
	require.NoError(t, err)
	_, err = one.Exec(t.Context(), "CREATE TABLE isolation_probe (id int)")
	require.NoError(t, err)
	require.NoError(t, one.Close(t.Context()))
	two, err := pgx.Connect(t.Context(), second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = two.Close(context.Background()) })
	var absent bool
	require.NoError(t, two.QueryRow(t.Context(), "SELECT to_regclass('isolation_probe') IS NULL").Scan(&absent))
	require.True(t, absent)
	cleanupFirst()
	require.NoError(t, two.Ping(t.Context()))
	base, err := pgx.Connect(t.Context(), raw)
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close(context.Background()) })
	require.NoError(t, base.Ping(t.Context()))
	// Reusing the old scratch name with a new OID cannot authorize deletion.
	firstConfig, err := pgx.ParseConfig(first)
	require.NoError(t, err)
	_, err = base.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{firstConfig.Database}.Sanitize())
	require.NoError(t, err)
	cleanupFirst()
	var exists bool
	require.NoError(t, base.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", firstConfig.Database).Scan(&exists))
	require.True(t, exists)
	_, err = base.Exec(t.Context(), "DROP DATABASE "+pgx.Identifier{firstConfig.Database}.Sanitize())
	require.NoError(t, err)
}
