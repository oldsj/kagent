package sdkrunner_test

import (
	"context"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
)

func TestParkedPromptWithoutClientIsIdle(t *testing.T) {
	// Given an A2A turn that raised a question for a human.
	f := newFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "AskUserQuestion", "input": map[string]any{}, "toolUseID": "t1"})
		}
	})
	sub, err := f.session.BeginTurn()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.Submit(context.Background(), "ask me", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	if item := next(t, sub); item.Kind != sdkrunner.ItemPerm {
		t.Fatalf("item = %+v, want the question", item)
	}

	// When the executor parks the task and no client is attached.
	f.session.EndTurn(sub)

	// Assert that the runtime reports idle, so the parked boundary can suspend
	// and a later wake without a client re-suspends.
	eventually(t, "idle report", func() bool { got := f.activity.Got(); return len(got) > 0 && !got[len(got)-1] })
}
