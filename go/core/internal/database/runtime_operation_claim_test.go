package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

func TestRuntimeOperationClaimLifetime(t *testing.T) {
	scenarios := []struct {
		name     string
		attempt  time.Duration
		wantNear time.Duration
	}{
		{name: "Given a 30m attempt deadline, when the attempt claims its operation, assert that the claim lasts the whole attempt", attempt: 30 * time.Minute, wantNear: 30 * time.Minute},
		{name: "Given no attempt deadline, when the attempt claims its operation, assert that the default bound applies", wantNear: DefaultRuntimeOperationTimeout},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			client, session := expirationFixture(t)
			operation, err := client.BeginSessionOperation(t.Context(), session.Id, apiv1.RuntimeOperation_RUNTIME_OPERATION_DELETE, true)
			require.NoError(t, err)
			ctx := t.Context()
			if scenario.attempt > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, scenario.attempt)
				defer cancel()
			}

			claimed, err := client.ClaimSessionOperation(ctx, session.Id, operation.ID, uuid.New())

			require.NoError(t, err)
			require.True(t, claimed)
			remaining, err := queryOne(t.Context(), client.db, `
				SELECT EXTRACT(EPOCH FROM executor_expires_at - clock_timestamp())::float8 FROM runtime_instance WHERE id = $1
			`, pgx.RowTo[float64], session.Id)
			require.NoError(t, err)
			require.InDelta(t, scenario.wantNear.Seconds(), remaining, 10)
		})
	}
}
