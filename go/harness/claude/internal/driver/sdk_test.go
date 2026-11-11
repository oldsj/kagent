package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

const sdkSessionID = "11111111-1111-4111-8111-111111111111"

func newSDKFixture(t *testing.T, handle func(*sdkrunnertest.Bridge, sdkrunnertest.Op)) (*SDKDriver, *sdkrunnertest.Launcher, *sdkrunnertest.Continuation) {
	t.Helper()
	launcher := sdkrunnertest.NewLauncher(handle)
	continuation := &sdkrunnertest.Continuation{}
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: launcher,
		Spec: func(sdkrunner.Profile) (sdkrunner.Spec, error) {
			return sdkrunner.Spec{Cwd: "/w", Executable: "claude"}, nil
		},
		Continuation: continuation, Dir: t.TempDir(), StopGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	base := NewProcessDriver(ProcessConfig{InterruptGrace: 200 * time.Millisecond, TurnTimeout: time.Minute})
	return NewSDKDriver(base, session), launcher, continuation
}

func sdkLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSDKDriverTranslatesItsTurnAndSkipsOtherResults(t *testing.T) {
	driver, _, continuation := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op != "user" {
			return
		}
		for _, line := range []string{
			`{"type":"system","subtype":"init","session_id":"` + sdkSessionID + `"}`,
			// A client's turn finishing first does not end this A2A turn.
			`{"type":"result","subtype":"success","result":"client","user_message_uuids":["someone-else"]}`,
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"tool1","name":"Bash","input":{"command":"ls"}}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool1","content":"a.txt"}]}}`,
			`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"done listing"}]}}`,
			`{"type":"result","subtype":"success","result":"done listing","user_message_uuids":["` + op.UUID + `"]}`,
		} {
			b.SDK(sdkLine(t, line))
		}
	})
	sink := &recordingSink{}
	outcome, err := driver.Run(context.Background(), runtime.Turn{Prompt: "list files"}, sink)
	if err != nil || outcome.Failure != nil || outcome.Pending != nil {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
	if sink.text.String() != "done listing" {
		t.Fatalf("text = %q", sink.text.String())
	}
	if len(sink.calls) != 1 || sink.calls[0].Name != "Bash" || len(sink.results) != 1 || sink.results[0].ID != "tool1" {
		t.Fatalf("calls = %+v results = %+v", sink.calls, sink.results)
	}
	if len(sink.sessions) != 1 || sink.sessions[0].ContinuationID != sdkSessionID || continuation.ID != sdkSessionID {
		t.Fatalf("sessions = %+v continuation = %q", sink.sessions, continuation.ID)
	}
}

func TestSDKDriverOverridesApplyToTheirTurnOnly(t *testing.T) {
	launcher := sdkrunnertest.NewLauncher(func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.SDK(sdkLine(t, `{"type":"result","subtype":"success","result":"ok","user_message_uuids":["`+op.UUID+`"]}`))
		}
	})
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: launcher,
		Spec: func(sdkrunner.Profile) (sdkrunner.Spec, error) {
			return sdkrunner.Spec{Cwd: "/w", Executable: "claude"}, nil
		},
		Continuation: &sdkrunnertest.Continuation{}, Dir: t.TempDir(), StopGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	base := NewProcessDriver(ProcessConfig{Model: "claude-sonnet-5-5", InterruptGrace: 200 * time.Millisecond, TurnTimeout: time.Minute})
	driver := NewSDKDriver(base, session)

	// controls returns the bridge ops each turn sends, in order, and clears them.
	seen := 0
	controls := func() string {
		var sequence []string
		ops := launcher.Ops()
		for _, op := range ops[seen:] {
			switch op.Op {
			case "model":
				sequence = append(sequence, "model="+op.Model)
			case "effort":
				sequence = append(sequence, "effort="+op.Effort)
			case "user":
				sequence = append(sequence, "user")
			}
		}
		seen = len(ops)
		return strings.Join(sequence, ",")
	}
	for _, step := range []struct {
		name string
		turn runtime.Turn
		want string
	}{
		{"override", runtime.Turn{Prompt: "x", Model: "claude-opus-5-5", Effort: "high"}, "model=claude-opus-5-5,effort=high,user"},
		// No keys: the configured model and the model's default effort, not
		// the previous turn's override.
		{"default", runtime.Turn{Prompt: "y"}, "model=claude-sonnet-5-5,effort=,user"},
		{"effort only", runtime.Turn{Prompt: "z", Effort: "low"}, "model=claude-sonnet-5-5,effort=low,user"},
	} {
		if outcome, err := driver.Run(context.Background(), step.turn, &recordingSink{}); err != nil || outcome.Failure != nil {
			t.Fatalf("%s: outcome = %+v err = %v", step.name, outcome, err)
		}
		if got := controls(); got != step.want {
			t.Fatalf("%s: bridge ops = %s, want %s", step.name, got, step.want)
		}
	}
}

func TestSDKDriverFailureResult(t *testing.T) {
	driver, _, _ := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.SDK(sdkLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","user_message_uuids":["`+op.UUID+`"]}`))
		}
	})
	outcome, err := driver.Run(context.Background(), runtime.Turn{Prompt: "x"}, &recordingSink{})
	if err != nil || outcome.Failure == nil {
		t.Fatalf("outcome = %+v err = %v, want a failure outcome", outcome, err)
	}
}

func TestSDKDriverApprovalParksAndRejectsMismatchedDecision(t *testing.T) {
	driver, _, _ := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "mcp__tools__deploy", "input": map[string]any{"env": "dev"}, "toolUseID": "call1"})
		}
	})
	sink := &recordingSink{}
	outcome, err := driver.Run(context.Background(), runtime.Turn{Prompt: "deploy"}, sink)
	if err != nil || outcome.Pending == nil {
		t.Fatalf("outcome = %+v err = %v, want pending approval", outcome, err)
	}
	request, ok := outcome.Pending.Request().(*runtime.ApprovalRequest)
	if !ok || request.ID != "p1" || request.CallID != "call1" || request.Name != "mcp__tools__deploy" {
		t.Fatalf("request = %+v", outcome.Pending.Request())
	}
	if _, err := outcome.Pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "wrong", Approved: true}, sink); err == nil {
		t.Fatal("a mismatched decision must be rejected")
	}
}

func TestSDKDriverApprovalResumeCompletes(t *testing.T) {
	uuids := make(chan string, 1)
	driver, launcher, _ := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		switch op.Op {
		case "user":
			uuids <- op.UUID
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "Bash", "input": map[string]any{}, "toolUseID": "call1"})
		case "perm":
			id := <-uuids
			b.SDK(sdkLine(t, `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"`+op.Behavior+`"}]}}`))
			b.SDK(sdkLine(t, `{"type":"result","subtype":"success","result":"ok","user_message_uuids":["`+id+`"]}`))
		}
	})
	sink := &recordingSink{}
	outcome, err := driver.Run(context.Background(), runtime.Turn{Prompt: "deploy"}, sink)
	if err != nil || outcome.Pending == nil {
		t.Fatalf("outcome = %+v err = %v", outcome, err)
	}
	final, err := outcome.Pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "p1", Approved: false, RejectionReason: "no"}, sink)
	if err != nil || final.Failure != nil || final.Pending != nil {
		t.Fatalf("final = %+v err = %v", final, err)
	}
	if sink.text.String() != "deny" {
		t.Fatalf("text = %q, want the denied continuation", sink.text.String())
	}
	var sawPerm bool
	for _, op := range launcher.Ops() {
		sawPerm = sawPerm || (op.Op == "perm" && op.ID == "p1" && op.Behavior == "deny")
	}
	if !sawPerm {
		t.Fatal("the decision did not reach the bridge")
	}
}

func TestSDKDriverCancelInterruptsAndReturns(t *testing.T) {
	uuids := make(chan string, 1)
	driver, launcher, _ := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		switch op.Op {
		case "user":
			uuids <- op.UUID
		case "interrupt":
			b.SDK(sdkLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"interrupted","user_message_uuids":["`+<-uuids+`"]}`))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := driver.Run(ctx, runtime.Turn{Prompt: "long"}, &recordingSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
	var interrupted bool
	for _, op := range launcher.Ops() {
		interrupted = interrupted || op.Op == "interrupt"
	}
	if !interrupted || launcher.Launches() != 1 {
		t.Fatalf("interrupted=%v launches=%d", interrupted, launcher.Launches())
	}
}

func TestSDKDriverBridgeExitFailsTheTurn(t *testing.T) {
	driver, _, _ := newSDKFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			go b.Exit()
		}
	})
	_, err := driver.Run(context.Background(), runtime.Turn{Prompt: "x"}, &recordingSink{})
	if !errors.Is(err, errRunnerExited) {
		t.Fatalf("err = %v, want runner exited", err)
	}
}

func TestSDKSpecProfiles(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, "mcp.json")
	_ = os.WriteFile(mcp, []byte(`{"mcpServers":{"tools":{"type":"http","url":"http://tools"},"kagent_hitl":{"type":"http","url":"http://127.0.0.1:1"}}}`), 0o600)
	prompt := filepath.Join(dir, "system-prompt.txt")
	_ = os.WriteFile(prompt, []byte("be brief"), 0o600)
	broker, err := NewApprovalBroker([]string{"tools"}, filepath.Join(dir, "adapter"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	d := NewProcessDriver(ProcessConfig{
		Executable: "sh", Workspace: "/data/workspace", Model: "opus", MCPConfigPath: mcp,
		SettingsPath: filepath.Join(dir, "settings.json"), ApprovalBroker: broker, AppendSystemPromptFile: prompt,
		PluginDirs: []string{"/p"}, DisallowedTools: []string{"Monitor"}, AgentsJSON: `{"a":{"description":"d","prompt":"p"}}`,
		Environment: []string{"CLAUDE_CONFIG_DIR=/data/claude", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1", "PATH=/usr/bin:/bin"},
	})
	managed, err := d.SDKSpec(sdkrunner.ProfileManaged, "/data/sdk-runner/home")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := managed.MCPServers["kagent_hitl"]; ok || managed.MCPServers["tools"] == nil {
		t.Fatalf("mcp = %v, want tools without the loopback approval server", managed.MCPServers)
	}
	// Both profiles carry the broker's deferral hooks and ask rules, as the
	// process driver's settings file does.
	wantSettings, err := broker.SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wantSettings), `"PreToolUse"`) || !strings.Contains(string(wantSettings), `"ask":["AskUserQuestion","mcp__tools__*"]`) {
		t.Fatalf("broker settings = %s, want its hooks and ask rules", wantSettings)
	}
	if managed.Settings != string(wantSettings) || !managed.StrictMCPConfig || managed.PermissionMode != "bypassPermissions" ||
		strings.Join(managed.SettingSources, ",") != "user" || managed.Env["CLAUDE_CONFIG_DIR"] != "/data/claude" ||
		managed.Env["CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"] != "1" || !filepath.IsAbs(managed.Executable) || managed.AppendSystemPrompt != "be brief" {
		t.Fatalf("managed = %+v", managed)
	}
	user, err := d.SDKSpec(sdkrunner.ProfileUser, "/data/sdk-runner/home")
	if err != nil {
		t.Fatal(err)
	}
	if user.Env["HOME"] != "/data/sdk-runner/home" || user.Env["CLAUDE_CONFIG_DIR"] != "" || user.StrictMCPConfig ||
		strings.Join(user.SettingSources, ",") != "user,project" || user.Env["CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"] != "1" {
		t.Fatalf("user = %+v", user)
	}
	if user.Settings != string(wantSettings) {
		t.Fatalf("user settings = %s, want the same hooks and ask rules beside the user's own", user.Settings)
	}
	// Without an approval boundary the managed profile runs no hook at all.
	bare := NewProcessDriver(ProcessConfig{Executable: "sh", Workspace: "/data/workspace"})
	for profile, want := range map[sdkrunner.Profile]string{sdkrunner.ProfileManaged: `{"disableAllHooks":true}`, sdkrunner.ProfileUser: `{}`} {
		spec, err := bare.SDKSpec(profile, "/data/sdk-runner/home")
		if err != nil || spec.Settings != want {
			t.Fatalf("%s settings without a broker = %q, %v; want %s", profile, spec.Settings, err, want)
		}
	}
}
