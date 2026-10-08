// Package dbtest provides test helpers for spinning up a Postgres container.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kagent-dev/kagent/go/core/pkg/migrations"
	testcontainers "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start starts a pgvector Postgres container and returns the connection string
// and a cleanup function. Callers are responsible for calling cleanup when done.
func Start(ctx context.Context) (connStr string, cleanup func(), err error) {
	// This explicit test-only opt-in never changes the normal container path.
	//nolint:forbidigo // Test fixture selection is intentionally process-local.
	if raw := os.Getenv("KAGENT_TEST_LOCAL_POSTGRES_URL"); raw != "" {
		return startLocal(ctx, raw)
	}
	pgContainer, err := tcpostgres.Run(ctx,
		"pgvector/pgvector:pg18-trixie",
		tcpostgres.WithDatabase("kagent_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("kagent"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", nil, fmt.Errorf("starting postgres container: %w", err)
	}

	connStr, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return "", nil, fmt.Errorf("getting connection string: %w", err)
	}

	cleanup = func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			fmt.Printf("warning: failed to terminate postgres container: %v\n", err)
		}
	}

	return connStr, cleanup, nil
}

// StartT starts a pgvector Postgres container and registers cleanup with t.Cleanup.
// Suitable for use in individual tests or test helpers that have a *testing.T.
func StartT(ctx context.Context, t *testing.T) string {
	t.Helper()

	connStr, cleanup, err := Start(ctx)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(cleanup)

	return connStr
}

// Migrate runs the embedded migrations against connStr and returns any error.
// If vectorEnabled is true the vector pass is also applied.
// Use MigrateT in tests that have a *testing.T; use Migrate in TestMain where no T is available.
func Migrate(connStr string, vectorEnabled bool) error {
	return migrations.RunUp(context.Background(), connStr, migrations.BuiltinSources(vectorEnabled))
}

// MigrateT runs the embedded migrations against connStr and calls t.Fatal on error.
// If vectorEnabled is true the vector pass is also applied.
func MigrateT(t *testing.T, connStr string, vectorEnabled bool) {
	t.Helper()
	if err := Migrate(connStr, vectorEnabled); err != nil {
		t.Fatalf("dbtest.MigrateT: %v", err)
	}
}

// localURL permits only the owned, loopback fixture and its explicit base DB.
// No host resolution, implicit libpq environment, SSL options or production URL.
func localURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" || u.Hostname() != "127.0.0.1" || u.Path != "/kagent_fixture" || u.Fragment != "" || u.User == nil || u.User.Username() != "postgres" {
		return nil, fmt.Errorf("local test fixture must be an explicit loopback kagent_fixture URL")
	}
	port, err := strconv.Atoi(u.Port())
	password, hasPassword := u.User.Password()
	if err != nil || port < 1024 || port > 65535 || !hasPassword || password != "kagent" || u.RawQuery != "sslmode=disable" {
		return nil, fmt.Errorf("unexpected local test fixture options")
	}
	return u, nil
}

func startLocal(ctx context.Context, raw string) (string, func(), error) {
	base, err := localURL(raw)
	if err != nil {
		return "", nil, err
	}
	conn, err := pgx.Connect(ctx, base.String())
	if err != nil {
		return "", nil, fmt.Errorf("connect owned local fixture: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	name := "kagent_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", nil, err
	}
	var oid uint32
	if err := conn.QueryRow(ctx, "SELECT oid FROM pg_database WHERE datname = $1", name).Scan(&oid); err != nil {
		return "", nil, err
	}
	base.Path = "/" + name
	fmt.Printf("owned local PostgreSQL scratch: %s oid=%d\n", name, oid)
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(cleanupCtx, raw)
		if err != nil {
			fmt.Printf("local fixture scratch cleanup unavailable: %s\n", name)
			return
		}
		defer func() { _ = admin.Close(cleanupCtx) }()
		var current uint32
		if err := admin.QueryRow(cleanupCtx, "SELECT oid FROM pg_database WHERE datname = $1", name).Scan(&current); err != nil || current != oid {
			return
		}
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			fmt.Printf("local fixture scratch cleanup failed: %s\n", name)
		}
	}
	return base.String(), cleanup, nil
}
