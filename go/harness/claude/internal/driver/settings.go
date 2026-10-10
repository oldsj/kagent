package driver

import (
	"encoding/json"
	"fmt"
)

// settingSources loads only Claude Code's user scope, the adapter-owned
// CLAUDE_CONFIG_DIR. Project and local settings can run commands outside any
// tool call (settings env reaches processes Claude starts, and helper keys
// such as otelHeadersHelper run commands), so a checkout must not supply
// them. The checkout's CLAUDE.md and .claude/rules load through --add-dir
// (see Args), and the adapter supplies its skills as a plugin.
const settingSources = "user"

// harnessSettings is the --settings layer. It outranks user and project
// settings, so hooks cannot run. Claude evaluates ask rules before
// allow rules from any source, so project settings cannot pre-approve the
// broker's protected tools. --strict-mcp-config keeps MCP servers to the
// adapter's --mcp-config.
type harnessSettings struct {
	DisableAllHooks bool                `json:"disableAllHooks"`
	Permissions     *harnessPermissions `json:"permissions,omitempty"`
}

type harnessPermissions struct {
	Ask []string `json:"ask"`
}

// HarnessSettingsJSON encodes the harness-owned settings layer. ask lists the
// permission rules that must reach the permission-prompt tool.
func HarnessSettingsJSON(ask []string) ([]byte, error) {
	settings := harnessSettings{DisableAllHooks: true}
	if len(ask) != 0 {
		settings.Permissions = &harnessPermissions{Ask: ask}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode Claude harness settings: %w", err)
	}
	return raw, nil
}

// defaultHarnessSettings is HarnessSettingsJSON(nil), passed inline when the
// adapter has not materialized an approval settings file.
const defaultHarnessSettings = `{"disableAllHooks":true}`
