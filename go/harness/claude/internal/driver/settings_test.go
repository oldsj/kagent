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

func TestArgsAppliesTurnModelAndEffortOverride(t *testing.T) {
	for name, test := range map[string]struct {
		config    ProcessConfig
		turn      runtime.Turn
		wantModel string
		wantFlags []string
	}{
		"no override uses configured model": {
			config: ProcessConfig{Model: "claude-sonnet-5"}, wantModel: "claude-sonnet-5",
		},
		"turn model overrides configured model": {
			config: ProcessConfig{Model: "claude-sonnet-5"}, turn: runtime.Turn{Model: "claude-opus-5"}, wantModel: "claude-opus-5",
		},
		"turn model without a configured default": {
			turn: runtime.Turn{Model: "claude-opus-5"}, wantModel: "claude-opus-5",
		},
		"turn effort is passed through": {
			turn: runtime.Turn{Effort: "high"}, wantFlags: []string{"--effort", "high"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := NewProcessDriver(test.config).Args(test.turn)
			if test.wantModel != "" {
				model := slices.Index(args, "--model")
				if model < 0 || model+1 >= len(args) || args[model+1] != test.wantModel {
					t.Fatalf("arguments do not pass model %q: %q", test.wantModel, args)
				}
			} else if slices.Contains(args, "--model") {
				t.Fatalf("arguments pass --model with no configured or overridden model: %q", args)
			}
			for i := 0; i+1 < len(test.wantFlags); i += 2 {
				flag, value := slices.Index(args, test.wantFlags[i]), test.wantFlags[i+1]
				if flag < 0 || flag+1 >= len(args) || args[flag+1] != value {
					t.Fatalf("arguments do not pass %s %q: %q", test.wantFlags[i], value, args)
				}
			}
			if test.turn.Effort == "" && slices.Contains(args, "--effort") {
				t.Fatalf("arguments pass --effort with no turn override: %q", args)
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
