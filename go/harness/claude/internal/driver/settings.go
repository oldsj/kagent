package driver

import (
	"encoding/json"
	"fmt"
)

// settingSources loads Claude Code's user scope, which is the adapter-owned
// CLAUDE_CONFIG_DIR, and the checkout's project scope: settings, CLAUDE.md
// memory, and skills. The uncommitted .claude/settings.local.json is not
// loaded.
const settingSources = "user,project"

// harnessSettings is the --settings layer. It outranks user and project
// settings, so their hooks cannot run. Claude evaluates ask rules before
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
