package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// The observe check compares config.toml byte for byte. Once the install has
// rendered the workspace trust entry, the pinned Codex leaves the file
// unchanged (TestPinnedCodexLeavesTrustedWorkspaceUnchanged), so every later
// observe, each turn's and a restart's, must pass. In 0.4 the missing entry
// made every turn after the first fail as "Workspace preparation is incomplete".
func TestSetupObservePassesOnceCodexTrustsTheWorkspace(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	durable := filepath.Dir(workspaceDir)
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	readOrigin, pushOrigin := apiworkspace.GitInstanceOrigins("mainloop")
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin}
	cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: apiworkspace.MCPInstanceOrigin("mainloop") + "/mcp"}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	store, err := continuation.New(filepath.Join(durable, "adapter"), "codex", func(string) error { return nil })
	require.NoError(t, err)
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(raw)
	require.NoError(t, err)
	setupDigest, err := apiworkspace.SetupDigest("agent")
	require.NoError(t, err)
	assignment := workspace.Preparation{Provider: "codex", Profile: "agent", SetupDigest: setupDigest, ConfigDigest: configDigest, MCPDigest: mcpDigest}
	installer := &SetupInstaller{Input: Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable}, Store: store}

	_, _, err = installer.Setup(t.Context(), assignment, false)
	require.NoError(t, err)
	configPath := filepath.Join(durable, "codex", "config.toml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(before), string(workspaceTrustEntry(workspaceDir)))
	for range 2 {
		_, _, err = installer.Setup(t.Context(), assignment, true)
		require.NoError(t, err)
	}
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
