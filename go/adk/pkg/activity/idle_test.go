package activity

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdleWhenServing(t *testing.T) {
	t.Run("Given a runtime without a Session identity, when it waits, assert that it reports nothing", func(t *testing.T) {
		sender := &fakeSender{}
		reporter := startReporter(t, sender)

		go reporter.IdleWhenServing(t.Context(), func() bool { return false }, time.Millisecond)

		time.Sleep(20 * time.Millisecond)
		require.Empty(t, sender.sent(), "a golden snapshot must not report")
	})

	t.Run("Given a wake that starts no turn, when the Session identity appears, assert that it reports idle once", func(t *testing.T) {
		sender := &fakeSender{}
		reporter := startReporter(t, sender)
		var serving atomic.Bool

		go reporter.IdleWhenServing(t.Context(), serving.Load, time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		serving.Store(true)

		require.Eventually(t, func() bool { return len(sender.sent()) == 1 }, time.Second, time.Millisecond)
		require.Equal(t, []bool{false}, sender.sent())
	})

	t.Run("Given a turn already marked busy, when the Session identity appears, assert that it is not reported idle", func(t *testing.T) {
		sender := &fakeSender{renewAfter: time.Hour}
		reporter := startReporter(t, sender)
		reporter.SetActive(true)
		require.Eventually(t, func() bool { return len(sender.sent()) == 1 }, time.Second, time.Millisecond)

		reporter.IdleWhenServing(t.Context(), func() bool { return true }, time.Millisecond)

		time.Sleep(20 * time.Millisecond)
		require.Equal(t, []bool{true}, sender.sent())
	})
}
