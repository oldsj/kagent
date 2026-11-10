package activity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeSender struct {
	mu         sync.Mutex
	reports    []bool
	errs       []error
	renewAfter time.Duration
}

func (s *fakeSender) ReportActivity(_ context.Context, active bool) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, active)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return 0, err
	}
	return s.renewAfter, nil
}

func (s *fakeSender) sent() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.reports...)
}

func startReporter(t *testing.T, sender *fakeSender) *Reporter {
	t.Helper()
	reporter := NewReporter(sender, nil)
	reporter.retryMin, reporter.retryMax = time.Millisecond, 4*time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return reporter
}

func TestReporterSendsNothingBeforeFirstTransition(t *testing.T) {
	sender := &fakeSender{renewAfter: time.Millisecond}
	startReporter(t, sender)
	time.Sleep(20 * time.Millisecond)
	require.Empty(t, sender.sent(), "no report during warm-up or golden snapshot creation")
}

func TestReporterRenewsWhileActiveAndStopsWhenIdle(t *testing.T) {
	sender := &fakeSender{renewAfter: 5 * time.Millisecond}
	reporter := startReporter(t, sender)
	reporter.SetActive(true)
	require.Eventually(t, func() bool { return len(sender.sent()) >= 3 }, time.Second, time.Millisecond, "active reports renew")
	for _, active := range sender.sent() {
		require.True(t, active)
	}
	reporter.SetActive(false)
	require.Eventually(t, func() bool { sent := sender.sent(); return !sent[len(sent)-1] }, time.Second, time.Millisecond)
	count := len(sender.sent())
	time.Sleep(30 * time.Millisecond)
	require.Len(t, sender.sent(), count, "idle is reported once and not renewed")
}

func TestReporterDeduplicatesRepeatedState(t *testing.T) {
	sender := &fakeSender{renewAfter: time.Hour}
	reporter := startReporter(t, sender)
	reporter.SetActive(true)
	require.Eventually(t, func() bool { return len(sender.sent()) == 1 }, time.Second, time.Millisecond)
	reporter.SetActive(true)
	reporter.SetActive(true)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, []bool{true}, sender.sent())
}

func TestReporterRetriesTransientFailures(t *testing.T) {
	sender := &fakeSender{renewAfter: time.Hour, errs: []error{
		status.Error(codes.Unavailable, "controller restarting"), errors.New("connection reset"),
	}}
	reporter := startReporter(t, sender)
	reporter.SetActive(false)
	require.Eventually(t, func() bool { return len(sender.sent()) == 3 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, []bool{false, false, false}, sender.sent(), "stops once delivered")
}

func TestReporterStopsAfterRepeatedPermissionDenied(t *testing.T) {
	denied := status.Error(codes.PermissionDenied, "runtime identity unavailable")
	sender := &fakeSender{renewAfter: time.Millisecond, errs: []error{denied, denied, denied, denied, denied}}
	reporter := startReporter(t, sender)
	reporter.SetActive(true)
	require.Eventually(t, func() bool { return len(sender.sent()) == deniedRetries+1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Len(t, sender.sent(), deniedRetries+1, "a denied Actor stops renewing")

	// The next transition reports again.
	reporter.SetActive(false)
	require.Eventually(t, func() bool { return len(sender.sent()) == deniedRetries+2 }, time.Second, time.Millisecond)
}

func TestReporterRecoversFromBriefDenialAfterWake(t *testing.T) {
	denied := status.Error(codes.PermissionDenied, "runtime identity unavailable")
	sender := &fakeSender{renewAfter: 5 * time.Millisecond, errs: []error{denied}}
	reporter := startReporter(t, sender)
	reporter.SetActive(true)
	require.Eventually(t, func() bool { return len(sender.sent()) >= 3 }, time.Second, time.Millisecond, "renewal continues after one denial")
}
