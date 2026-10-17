package continuation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type freshSource struct {
	session string
	fresh   bool
	err     error
	asked   int
}

func (s *freshSource) SessionID() (string, error) { return s.session, nil }
func (s *freshSource) FreshConversation(context.Context) (bool, error) {
	s.asked++
	return s.fresh, s.err
}

// freshFixture lays out a restored /data that holds a bound conversation, its
// transcript, and files a fork must keep.
func freshFixture(t *testing.T, source *freshSource) (*Store, string) {
	t.Helper()
	data := t.TempDir()
	store, err := New(filepath.Join(data, "adapter"), "claude", func(string) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Bind("thread-1"))
	for path, content := range map[string]string{
		"claude/projects/work/thread-1.jsonl": "transcript",
		"claude/history.jsonl":                "prompts",
		"claude/settings.json":                "settings",
		"workspace/main.go":                   "package main",
		"cache/modules/x":                     "cached",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(data, path)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(data, path), []byte(content), 0o600))
	}
	store.RequireFresh(Fresh{
		Source: source, MarkerDir: filepath.Join(data, ".kagent"),
		Clear: []string{filepath.Join(data, "claude/projects"), filepath.Join(data, "claude/history.jsonl")},
	})
	return store, data
}

func TestSettleDiscardsConversationOfFreshFork(t *testing.T) {
	source := &freshSource{session: "fork-1", fresh: true}
	store, data := freshFixture(t, source)

	require.NoError(t, store.Settle(t.Context()))
	_, bound, err := store.Load()
	require.NoError(t, err)
	require.False(t, bound, "the fork must not resume the source's native conversation")
	require.NoDirExists(t, filepath.Join(data, "claude/projects"))
	require.NoFileExists(t, filepath.Join(data, "claude/history.jsonl"))
	for _, kept := range []string{"claude/settings.json", "workspace/main.go", "cache/modules/x"} {
		require.FileExists(t, filepath.Join(data, kept))
	}

	// The fork's own conversation survives later turns and a restore of its data.
	require.NoError(t, store.Bind("thread-2"))
	require.NoError(t, store.Settle(t.Context()))
	id, bound, err := store.Load()
	require.NoError(t, err)
	require.True(t, bound)
	require.Equal(t, "thread-2", id)
	require.Equal(t, 1, source.asked, "a settled Session never asks the control plane again")
}

func TestSettleLeavesDefaultForkAlone(t *testing.T) {
	source := &freshSource{session: "fork-1", fresh: false}
	store, data := freshFixture(t, source)

	require.NoError(t, store.Settle(t.Context()))
	id, bound, err := store.Load()
	require.NoError(t, err)
	require.True(t, bound)
	require.Equal(t, "thread-1", id)
	require.FileExists(t, filepath.Join(data, "claude/projects/work/thread-1.jsonl"))
	require.FileExists(t, filepath.Join(data, "claude/history.jsonl"))
	require.NoError(t, store.Settle(t.Context()))
	require.Equal(t, 1, source.asked)
}

// A fork of a fresh fork carries its parent's marker and must still start fresh.
func TestSettleHandlesEachSessionOnce(t *testing.T) {
	source := &freshSource{session: "fork-1", fresh: true}
	store, _ := freshFixture(t, source)
	require.NoError(t, store.Settle(t.Context()))
	require.NoError(t, store.Bind("thread-2"))

	source.session = "fork-2"
	require.NoError(t, store.Settle(t.Context()))
	_, bound, err := store.Load()
	require.NoError(t, err)
	require.False(t, bound)
	require.Equal(t, 2, source.asked)
}

// An unanswered question must fail the turn, never continue the old conversation.
func TestSettleFailsClosed(t *testing.T) {
	source := &freshSource{session: "fork-1", err: errors.New("control plane unavailable")}
	store, data := freshFixture(t, source)

	require.Error(t, store.Settle(t.Context()))
	id, bound, err := store.Load()
	require.NoError(t, err)
	require.True(t, bound)
	require.Equal(t, "thread-1", id)
	require.NoFileExists(t, filepath.Join(data, ".kagent", freshMarker))

	source.err, source.fresh = nil, true
	require.NoError(t, store.Settle(t.Context()))
	_, bound, err = store.Load()
	require.NoError(t, err)
	require.False(t, bound)
}

func TestSettleWithoutFreshnessDoesNothing(t *testing.T) {
	store, err := New(t.TempDir(), "claude", func(string) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Settle(t.Context()))
}
