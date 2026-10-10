package driver

import (
	"slices"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestHarnessSettingsJSON(t *testing.T) {
	for name, test := range map[string]struct {
		ask  []string
		want string
	}{
		"hooks only":    {want: defaultHarnessSettings},
		"approval asks": {ask: []string{"mcp__a__*", "mcp__b__*"}, want: `{"disableAllHooks":true,"permissions":{"ask":["mcp__a__*","mcp__b__*"]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := HarnessSettingsJSON(test.ask)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != test.want {
				t.Fatalf("HarnessSettingsJSON() = %s, want %s", raw, test.want)
			}
		})
	}
}

func TestArgsLoadUserSettingsAndWorkspaceContext(t *testing.T) {
	for name, test := range map[string]struct {
		config       ProcessConfig
		settings     string
		promptBridge bool
	}{
		"no approval broker": {config: ProcessConfig{Workspace: "/work"}, settings: defaultHarnessSettings},
		"approval broker": {
			config: ProcessConfig{
				Workspace: "/work", ApprovalBroker: fakeApprovalBroker(), SettingsPath: "/run/claude/settings.json",
				PermissionPromptTool: "mcp__kagent_hitl__approve",
			},
			settings: "/run/claude/settings.json", promptBridge: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := NewProcessDriver(test.config).Args(runtime.Turn{})
			sources := slices.Index(args, "--setting-sources")
			if sources < 0 || sources+1 >= len(args) || args[sources+1] != "user" {
				t.Fatalf("arguments do not load only user settings: %q", args)
			}
			if addDir := slices.Index(args, "--add-dir"); addDir < 0 || addDir+1 >= len(args) || args[addDir+1] != "/work" {
				t.Fatalf("arguments do not add the workspace for its context: %q", args)
			}
			settings := slices.Index(args, "--settings")
			if settings < 0 || settings+1 >= len(args) || args[settings+1] != test.settings {
				t.Fatalf("arguments do not pass harness settings %q: %q", test.settings, args)
			}
			if got := slices.Contains(args, "--permission-prompt-tool"); got != test.promptBridge {
				t.Fatalf("permission prompt bridge = %t, want %t: %q", got, test.promptBridge, args)
			}
		})
	}
}
