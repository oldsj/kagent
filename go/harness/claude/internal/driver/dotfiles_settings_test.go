package driver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFilterDotfilesSettingsKeepsOnlyRuleLists(t *testing.T) {
	// Fictional owner settings, with keys the harness must not take.
	raw := []byte(`{
		"$schema": "https://json.schemastore.org/claude-code-settings.json",
		"autoMode": {"environment": ["Trusted repo: example/fictional"], "soft_deny": ["$defaults", "Deleting example buckets"], "classifyAllShell": false, "allow": []},
		"permissions": {"allow": ["Bash(make test)"], "deny": ["Read(**/.env)"], "defaultMode": "bypassPermissions", "disableBypassPermissionsMode": "allow"},
		"hooks": {"PreToolUse": []},
		"env": {"X": "y"}
	}`)
	kept, dropped, err := FilterDotfilesSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"autoMode":    map[string]any{"environment": []string{"Trusted repo: example/fictional"}, "soft_deny": []string{"$defaults", "Deleting example buckets"}},
		"permissions": map[string]any{"allow": []string{"Bash(make test)"}, "deny": []string{"Read(**/.env)"}},
	}
	if !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %#v", kept)
	}
	if want := []string{"autoMode.classifyAllShell", "env", "hooks", "permissions.defaultMode", "permissions.disableBypassPermissionsMode"}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped = %v, want %v", dropped, want)
	}

	for name, bad := range map[string]string{
		"not an object":      `["autoMode"]`,
		"null":               `null`,
		"trailing data":      `{} {}`,
		"section not object": `{"autoMode": ["x"]}`,
		"list not strings":   `{"permissions": {"deny": [1]}}`,
		"empty entry":        `{"autoMode": {"allow": [" "]}}`,
	} {
		if _, _, err := FilterDotfilesSettings([]byte(bad)); err == nil {
			t.Errorf("%s: FilterDotfilesSettings() accepted %s", name, bad)
		}
	}
}

// The harness layer wins every key but the rule lists, which are joined, so
// dotfiles cannot undo a lock, a hook or an approval rule.
func TestMergeDotfilesSettingsKeepsTheHarnessLayer(t *testing.T) {
	harness := `{"permissions":{"ask":["AskUserQuestion","mcp__protected__*"],"disableBypassPermissionsMode":"disable"},"hooks":{"PreToolUse":[{"matcher":"*"}]}}`
	dotfiles, _, err := FilterDotfilesSettings([]byte(`{"autoMode":{"environment":["Trusted repo: example/fictional"]},"permissions":{"ask":["Bash(example deploy *)","AskUserQuestion"],"allow":["mcp__protected__write"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeDotfilesSettings(harness, dotfiles)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		AutoMode    map[string][]string `json:"autoMode"`
		Permissions struct {
			Ask     []string `json:"ask"`
			Allow   []string `json:"allow"`
			Disable string   `json:"disableBypassPermissionsMode"`
		} `json:"permissions"`
		Hooks map[string][]map[string]string `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatal(err)
	}
	if got.Permissions.Disable != "disable" || len(got.Hooks["PreToolUse"]) != 1 || got.Hooks["PreToolUse"][0]["matcher"] != "*" {
		t.Fatalf("the harness layer changed: %s", merged)
	}
	if !reflect.DeepEqual(got.Permissions.Ask, []string{"Bash(example deploy *)", "AskUserQuestion", "mcp__protected__*"}) {
		t.Fatalf("ask = %v", got.Permissions.Ask)
	}
	if !reflect.DeepEqual(got.Permissions.Allow, []string{"mcp__protected__write"}) || !reflect.DeepEqual(got.AutoMode["environment"], []string{"Trusted repo: example/fictional"}) {
		t.Fatalf("dotfiles lists lost: %s", merged)
	}

	if unchanged, err := MergeDotfilesSettings(harness, nil); err != nil || unchanged != harness {
		t.Fatalf("empty dotfiles changed the layer: %q, %v", unchanged, err)
	}
	if _, err := MergeDotfilesSettings("not json", dotfiles); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("bad layer error = %v", err)
	}
}
