package database

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

// The dotfiles are recorded on create but, being updatable, are not part of a
// create request's identity: a retry returns the Session as it is now.
func TestSessionDotfilesPersistenceAndRetry(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	ctx := t.Context()
	sessionFixture(t, c, ctx, "team-a", "revision-1", "assistant", "kagent")
	request := newSessionRequest(uuid.NewString(), "assistant", "kagent", "")
	request.Dotfiles = testDotfiles("a")
	saved, created, err := c.CreateSession(ctx, request, "dotfiles")
	require.NoError(t, err)
	require.True(t, created)
	read, err := c.GetSessionByID(ctx, saved.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(request.Dotfiles, read.Dotfiles))

	for _, retry := range []*apiv1.Dotfiles{request.Dotfiles, testDotfiles("b"), nil} {
		again := proto.CloneOf(request)
		again.Dotfiles = retry
		got, created, err := c.CreateSession(ctx, again, "dotfiles")
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, saved.Id, got.Id)
		require.True(t, proto.Equal(request.Dotfiles, got.Dotfiles), "a retry changed the dotfiles")
	}
}

func testDotfiles(digit string) *apiv1.Dotfiles {
	digest := "sha256:" + strings.Repeat(digit, 64)
	return &apiv1.Dotfiles{
		Url: "https://mcp.example.test/dotfiles/" + digest, Digest: digest,
		Repository: "example/agent-dotfiles", Commit: strings.Repeat("c", 40), Path: "home/.agents",
	}
}

// An update is a compare-and-swap on the digest: it applies at the previous
// digest, succeeds unchanged when the Session already holds the dotfiles, and
// conflicts otherwise, so a stale retry cannot undo a newer update.
func TestUpdateSessionDotfiles(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	ctx := t.Context()
	sessionFixture(t, c, ctx, "team-a", "revision-1", "assistant", "kagent")
	request := newSessionRequest(uuid.NewString(), "assistant", "kagent", "")
	request.Dotfiles = testDotfiles("a")
	saved, _, err := c.CreateSession(ctx, request, "dotfiles-update")
	require.NoError(t, err)
	owner := saved.GetCreator()
	pass := func(*apiv1.Session) error { return nil }
	digest := func(d string) string { return "sha256:" + strings.Repeat(d, 64) }

	b := testDotfiles("b")
	updated, err := c.UpdateSessionDotfiles(ctx, saved.Id, owner, digest("a"), b, pass)
	require.NoError(t, err)
	require.True(t, proto.Equal(b, updated.Dotfiles))
	read, err := c.GetSessionByID(ctx, saved.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(b, read.Dotfiles))

	// A retry of the same update succeeds without a change.
	again, err := c.UpdateSessionDotfiles(ctx, saved.Id, owner, digest("a"), b, func(*apiv1.Session) error {
		t.Fatal("an applied update was checked again")
		return nil
	})
	require.NoError(t, err)
	require.True(t, proto.Equal(b, again.Dotfiles))

	// A stale update conflicts and changes nothing.
	_, err = c.UpdateSessionDotfiles(ctx, saved.Id, owner, digest("a"), testDotfiles("d"), pass)
	require.ErrorIs(t, err, ErrConflict)

	// The check sees the locked Session and its refusal is returned as is.
	refused := errors.New("refused")
	_, err = c.UpdateSessionDotfiles(ctx, saved.Id, owner, digest("b"), testDotfiles("e"), func(s *apiv1.Session) error {
		require.Equal(t, saved.Id, s.Id)
		return refused
	})
	require.ErrorIs(t, err, refused)

	_, err = c.UpdateSessionDotfiles(ctx, saved.Id, "someone-else", digest("b"), testDotfiles("e"), pass)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = c.UpdateSessionDotfiles(ctx, uuid.NewString(), owner, "", testDotfiles("e"), pass)
	require.ErrorIs(t, err, ErrNotFound)
	read, err = c.GetSessionByID(ctx, saved.Id)
	require.NoError(t, err)
	require.True(t, proto.Equal(b, read.Dotfiles))

	// A Session created without dotfiles takes them at the empty digest.
	plain, _, err := c.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", ""), "dotfiles-none")
	require.NoError(t, err)
	_, err = c.UpdateSessionDotfiles(ctx, plain.Id, owner, "", testDotfiles("f"), pass)
	require.NoError(t, err)
}
