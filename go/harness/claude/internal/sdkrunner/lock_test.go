package sdkrunner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
)

func lockedSpec() *sdkrunner.Spec {
	return &sdkrunner.Spec{
		Cwd: "/data/workspace", Executable: "/usr/local/bin/claude", Model: "opus",
		PermissionMode: "auto", LockedPermissionMode: "auto",
	}
}

func modeOps(ops []sdkrunnertest.Op) []sdkrunnertest.Op {
	var modes []sdkrunnertest.Op
	for _, op := range ops {
		if op.Op == "mode" {
			modes = append(modes, op)
		}
	}
	return modes
}

func TestLockedPermissionMode(t *testing.T) {
	t.Run("Given a locked Harness, when a client sets the permission mode, assert that it is refused and never reaches Claude", func(t *testing.T) {
		f := newSpecFixture(t, lockedSpec(), sdkrunnertest.Echo("77777777-7777-7777-7777-777777777777"))
		conn := attachClient(t, f.session)

		reply := conn.Send(`{"k":"mode","id":"r1","mode":"bypassPermissions"}`)

		if reply["k"] != "err" || !strings.Contains(reply["text"].(string), "locked") {
			t.Fatalf("reply = %v, want an error saying the mode is locked", reply)
		}
		if modes := modeOps(f.launcher.Ops()); len(modes) != 0 {
			t.Fatalf("mode ops sent to Claude = %v, want none", modes)
		}
	})

	t.Run("Given a locked Harness, when the harness itself sets the mode, assert that ErrPermissionModeLocked is returned", func(t *testing.T) {
		f := newSpecFixture(t, lockedSpec(), sdkrunnertest.Echo("77777777-7777-7777-7777-777777777777"))

		err := f.session.Control(context.Background(), "mode", "default", "")

		if !errors.Is(err, sdkrunner.ErrPermissionModeLocked) {
			t.Fatalf("err = %v, want ErrPermissionModeLocked", err)
		}
	})

	t.Run("Given an unlocked Session whose mode a client changed, when the bridge restarts under a lock, assert that it starts in the locked mode", func(t *testing.T) {
		spec := lockedSpec()
		spec.LockedPermissionMode = ""
		f := newSpecFixture(t, spec, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
			if op.Op == "mode" {
				b.Emit(map[string]any{"k": "ack", "what": "mode", "mode": op.Mode, "ref": op.Ref})
			}
		})
		restart := func(launches int) *sdkrunnertest.Bridge {
			t.Helper()
			eventually(t, "a bridge start", func() bool {
				_ = f.session.Start(context.Background()) // a no-op until the exit is seen
				return f.launcher.Launches() == launches
			})
			return f.launcher.Started(wait)
		}
		if err := f.session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		first := f.launcher.Started(wait)
		eventually(t, "the client's mode to stick", func() bool {
			// Unlocked, a restart keeps the client's mode once its ack is in.
			if err := f.session.Control(context.Background(), "mode", "default", ""); err != nil {
				t.Fatal(err)
			}
			first.Exit()
			first = restart(f.launcher.Launches() + 1)
			return first.Spec.PermissionMode == "default"
		})
		spec.LockedPermissionMode = "auto"
		first.Exit()

		second := restart(f.launcher.Launches() + 1)

		if second.Spec.PermissionMode != "auto" {
			t.Fatalf("restart mode = %q, want the locked mode auto, not the client's default", second.Spec.PermissionMode)
		}
	})

	t.Run("Given a locked Harness, when Claude reports bypassPermissions, assert that the bridge is stopped", func(t *testing.T) {
		f := newSpecFixture(t, lockedSpec(), func(*sdkrunnertest.Bridge, sdkrunnertest.Op) {})
		if err := f.session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridge := f.launcher.Started(wait)
		stopped := make(chan struct{})
		go func() { _ = bridge.Wait(); close(stopped) }()

		bridge.SDK(map[string]any{"type": "system", "subtype": "status", "permissionMode": "bypassPermissions"})

		select {
		case <-stopped:
		case <-time.After(wait):
			t.Fatal("the bridge kept running in bypassPermissions on a locked Harness")
		}
	})

	t.Run("Given a locked Harness, when Claude reports plan mode, assert that the bridge keeps running", func(t *testing.T) {
		f := newSpecFixture(t, lockedSpec(), func(*sdkrunnertest.Bridge, sdkrunnertest.Op) {})
		if err := f.session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		bridge := f.launcher.Started(wait)
		stopped := make(chan struct{})
		go func() { _ = bridge.Wait(); close(stopped) }()

		bridge.SDK(map[string]any{"type": "system", "subtype": "status", "permissionMode": "plan"})

		select {
		case <-stopped:
			t.Fatal("a stricter mode stopped the bridge")
		case <-time.After(100 * time.Millisecond):
		}
	})
}
