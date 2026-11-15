package sdkrunner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
)

// StopIfIdle restarts the bridge between turns, so the next start prepares
// again (new agent dotfiles) and resumes the conversation, and leaves a bridge
// that is working alone.
func TestStopIfIdleStopsOnlyAnIdleBridge(t *testing.T) {
	const conversation = "33333333-3333-3333-3333-333333333333"
	f := newFixture(t, sdkrunnertest.Echo(conversation))
	ctx := context.Background()
	if stopped, err := f.session.StopIfIdle(ctx); err != nil || !stopped || f.launcher.Launches() != 0 {
		t.Fatalf("with no bridge: stopped=%v err=%v launches=%d", stopped, err, f.launcher.Launches())
	}

	sub, err := f.session.BeginTurn()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.Submit(ctx, "one", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	for item := next(t, sub); item.Kind != sdkrunner.ItemSDK || !strings.Contains(string(item.Raw), `"result"`); item = next(t, sub) {
	}
	// A registered turn is work: the bridge is left running.
	if stopped, err := f.session.StopIfIdle(ctx); err != nil || stopped {
		t.Fatalf("during a turn: stopped=%v err=%v", stopped, err)
	}
	f.session.EndTurn(sub)
	eventually(t, "idle", func() bool { return !f.session.Busy() })

	if stopped, err := f.session.StopIfIdle(ctx); err != nil || !stopped {
		t.Fatalf("when idle: stopped=%v err=%v", stopped, err)
	}
	sub, err = f.session.BeginTurn()
	if err != nil {
		t.Fatal(err)
	}
	defer f.session.EndTurn(sub)
	if err := f.session.Submit(ctx, "two", "u2", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	f.launcher.Started(wait)
	second := f.launcher.Started(wait)
	if second == nil || second.Spec.Resume != conversation || f.prepared != 2 {
		t.Fatalf("restart = %+v, prepared %d times; want a prepared start resuming the conversation", second, f.prepared)
	}
}
