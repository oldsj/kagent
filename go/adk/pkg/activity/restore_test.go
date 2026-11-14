package activity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportAfterRestores(t *testing.T) {
	t.Run("Given an idle runtime that already reported, when a restore projects its identity file anew, assert that it reports idle again", func(t *testing.T) {
		sender := &fakeSender{}
		reporter := startReporter(t, sender)
		identity := writeIdentity(t, filepath.Join(t.TempDir(), "name"))
		reporter.SetActive(false)
		require.Eventually(t, func() bool { return len(sender.sent()) == 1 }, time.Second, time.Millisecond)
		go reporter.ReportAfterRestores(t.Context(), identity, time.Millisecond)
		time.Sleep(10 * time.Millisecond)

		replaceIdentity(t, identity)

		require.Eventually(t, func() bool { return len(sender.sent()) == 2 }, time.Second, time.Millisecond)
		require.Equal(t, []bool{false, false}, sender.sent())
	})

	t.Run("Given a busy runtime, when a restore projects its identity file anew, assert that it reports busy again", func(t *testing.T) {
		sender := &fakeSender{renewAfter: time.Hour}
		reporter := startReporter(t, sender)
		identity := writeIdentity(t, filepath.Join(t.TempDir(), "name"))
		reporter.SetActive(true)
		require.Eventually(t, func() bool { return len(sender.sent()) == 1 }, time.Second, time.Millisecond)
		go reporter.ReportAfterRestores(t.Context(), identity, time.Millisecond)
		time.Sleep(10 * time.Millisecond)

		replaceIdentity(t, identity)

		require.Eventually(t, func() bool { return len(sender.sent()) == 2 }, time.Second, time.Millisecond)
		require.Equal(t, []bool{true, true}, sender.sent())
	})

	t.Run("Given a golden snapshot without an identity, when the identity file first appears, assert that no restore is reported", func(t *testing.T) {
		sender := &fakeSender{}
		reporter := startReporter(t, sender)
		identity := filepath.Join(t.TempDir(), "name")
		go reporter.ReportAfterRestores(t.Context(), identity, time.Millisecond)
		time.Sleep(10 * time.Millisecond)

		writeIdentity(t, identity)

		time.Sleep(20 * time.Millisecond)
		require.Empty(t, sender.sent(), "IdleWhenServing owns the first identity")
	})
}

func writeIdentity(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("session-00000000-0000-0000-0000-000000000001\n"), 0o644))
	return path
}

// replaceIdentity projects the same content as a new file, as a restore does.
func replaceIdentity(t *testing.T, path string) {
	t.Helper()
	next := path + ".next"
	writeIdentity(t, next)
	require.NoError(t, os.Rename(next, path))
}
