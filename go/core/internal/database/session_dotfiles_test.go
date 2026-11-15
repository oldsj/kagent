package database

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

// The dotfiles are part of a create request's identity: a replay with the same
// bundle returns the Session, and one with another bundle conflicts.
func TestSessionDotfilesPersistenceAndRetry(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	ctx := t.Context()
	sessionFixture(t, c, ctx, "team-a", "revision-1", "assistant", "kagent")
	request := newSessionRequest(uuid.NewString(), "assistant", "kagent", "")
	request.Dotfiles = &apiv1.Dotfiles{
		Url:    "https://mcp.example.test/dotfiles/x",
		Digest: "sha256:" + strings.Repeat("a", 64), Repository: "example/agent-dotfiles", Commit: strings.Repeat("c", 40),
	}
	saved, created, err := c.CreateSession(ctx, request, "dotfiles")
	require.NoError(t, err)
	require.True(t, created)
	read, err := c.GetSessionByID(ctx, saved.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(request.Dotfiles, read.Dotfiles))

	retry, created, err := c.CreateSession(ctx, request, "dotfiles")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, saved.Id, retry.Id)

	changed := proto.CloneOf(request)
	changed.Dotfiles.Digest = "sha256:" + strings.Repeat("b", 64)
	_, _, err = c.CreateSession(ctx, changed, "dotfiles")
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	none := proto.CloneOf(request)
	none.Dotfiles = nil
	_, _, err = c.CreateSession(ctx, none, "dotfiles")
	require.ErrorIs(t, err, ErrIdempotencyConflict)
}
