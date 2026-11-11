package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// preparedConfig is a Claude configuration ConfigDigests accepts: the default
// Mainloop instance's Git proxies and its MCP server.
func preparedConfig(protected bool) config.Config {
	cfg := config.Production("fixture-model", "original instructions")
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
	cfg.MCPServers = map[string]config.MCPServer{"mainloop": {
		Type: "http", URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp",
		Headers: map[string]string{"Authorization": "${MAINLOOP_MCP_AUTHORIZATION}"}, RequireApproval: protected,
	}}
	return cfg
}

func assignmentFor(t *testing.T, raw []byte, profile string) workspace.Preparation {
	t.Helper()
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(raw)
	require.NoError(t, err)
	setup, err := apiworkspace.SetupDigest(profile)
	require.NoError(t, err)
	return workspace.Preparation{Provider: "claude", Profile: profile, SetupDigest: setup, ConfigDigest: configDigest, MCPDigest: mcpDigest}
}

func TestPreparationSetupHasNoNativeHistory(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			data := t.TempDir()
			cfg := preparedConfig(false)
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			store, err := continuation.New(filepath.Join(data, "adapter"), "claude", validateSessionID)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(data, ".kagent"), 0o700))
			ephemeral := filepath.Join(data, "ephemeral")
			installer := &adapter.SetupInstaller{Input: adapter.Input{ConfigJSON: raw, Workspace: filepath.Join(data, "workspace"), DurableDir: data, EphemeralDir: ephemeral}, Store: store}
			t.Cleanup(func() { _ = installer.Close() })
			assignment := assignmentFor(t, raw, profile)

			runner, hook, err := installer.Setup(t.Context(), assignment, false)
			require.NoError(t, err)
			require.NotNil(t, runner)
			require.Equal(t, "append_system_prompt", hook)
			standing, err := apiworkspace.Standing(profile)
			require.NoError(t, err)
			prompt, err := os.ReadFile(filepath.Join(ephemeral, "system-prompt.txt"))
			require.NoError(t, err)
			require.Equal(t, cfg.AppendSystemPrompt+"\n"+standing, string(prompt))
			_, started, err := store.Load()
			require.NoError(t, err)
			require.False(t, started)

			_, hook, err = installer.Setup(t.Context(), assignment, true)
			require.NoError(t, err)
			require.Equal(t, "append_system_prompt", hook)

			wrongProvider := assignment
			wrongProvider.Provider = "codex"
			_, _, err = installer.Setup(t.Context(), wrongProvider, true)
			require.Error(t, err)
			assignment.SetupDigest = strings.Repeat("0", 64)
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			assignment.SetupDigest, err = apiworkspace.SetupDigest(profile)
			require.NoError(t, err)
			for _, name := range []string{"mcp.json", "settings.json", "system-prompt.txt"} {
				path := filepath.Join(ephemeral, name)
				original, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte("changed native setup"), 0o600))
				_, _, err = installer.Setup(t.Context(), assignment, true)
				require.Error(t, err, name)
				require.NoError(t, os.WriteFile(path, original, 0o600))
			}
			require.NoError(t, store.Bind("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
			_, _, err = installer.Setup(t.Context(), assignment, false)
			require.Error(t, err, "setup never applies over native history")
		})
	}
}

func TestPreparationRuntimeRecoveryPreservesApprovalAndHistory(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			for _, protected := range []bool{false, true} {
				t.Run(fmt.Sprintf("approval=%t", protected), func(t *testing.T) {
					data := t.TempDir()
					cfg := preparedConfig(protected)
					cfg.ClaudeExecutable = filepath.Join(data, "fake-claude")
					log := filepath.Join(data, "native-calls")
					require.NoError(t, os.WriteFile(cfg.ClaudeExecutable, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '"+cfg.ExpectedClaudeVersion+" (Claude Code)'; exit 0; fi\nprintf 'native call\\n' >> '"+log+"'\nexit 42\n"), 0o700))
					raw, err := json.Marshal(cfg)
					require.NoError(t, err)
					input := adapter.Input{ConfigJSON: raw, Workspace: filepath.Join(data, "workspace"), DurableDir: data, EphemeralDir: "/tmp/kagent-claude", Environment: []string{"PATH=" + os.Getenv("PATH")}}
					initial, err := adapter.New(t.Context(), input)
					require.NoError(t, err)
					readMCP := func() map[string]config.MCPServer {
						t.Helper()
						actual, err := os.ReadFile(filepath.Join(input.EphemeralDir, "mcp.json"))
						require.NoError(t, err)
						var document struct {
							Servers map[string]config.MCPServer `json:"mcpServers"`
						}
						require.NoError(t, json.Unmarshal(actual, &document))
						require.Equal(t, cfg.MCPServers["mainloop"].URL, document.Servers["mainloop"].URL)
						require.Equal(t, cfg.MCPServers["mainloop"].Headers, document.Servers["mainloop"].Headers)
						return document.Servers
					}
					oldBridge := readMCP()["kagent_hitl"].URL
					store, err := continuation.New(filepath.Join(data, "adapter"), "claude", validateSessionID)
					require.NoError(t, err)
					installer := &adapter.SetupInstaller{Input: input, Store: store, Runner: initial}
					t.Cleanup(func() { _ = installer.Close() })
					assignment := assignmentFor(t, raw, profile)
					_, _, err = installer.Setup(t.Context(), assignment, false)
					require.NoError(t, err)

					// Setup replaces the startup driver and its approval bridge.
					bridge := readMCP()["kagent_hitl"].URL
					require.NotEqual(t, oldBridge, bridge)
					_, err = http.Get(oldBridge) //nolint:noctx // Local loopback probe.
					require.Error(t, err, "replaced startup broker must close")
					if protected {
						response, err := http.Get(bridge) //nolint:noctx // Local loopback probe.
						require.NoError(t, err)
						require.Equal(t, http.StatusUnauthorized, response.StatusCode)
						require.NoError(t, response.Body.Close())
					}
					setupPath := filepath.Join(data, ".kagent", "native-setup.json")
					originalSetup, err := os.ReadFile(setupPath)
					require.NoError(t, err)
					// Existing continuation is retained across startup reconstruction.
					require.NoError(t, store.Bind("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
					history, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
					require.NoError(t, err)
					require.NoError(t, installer.Close())
					_, err = http.Get(bridge) //nolint:noctx // Local loopback probe.
					require.Error(t, err, "executor closer must own configured broker")

					// A restarted Actor has lost its ephemeral directory.
					for _, name := range []string{"mcp.json", "settings.json", "system-prompt.txt"} {
						require.NoError(t, os.Remove(filepath.Join(input.EphemeralDir, name)))
					}
					observer := &adapter.SetupInstaller{Input: input, Store: store}
					_, _, err = observer.Setup(t.Context(), assignment, true)
					require.Error(t, err, "read-only observation cannot reinstall absent files")
					require.NoFileExists(t, filepath.Join(input.EphemeralDir, "mcp.json"))

					_, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: input.Environment, Workspace: &startupWorkspace{}})
					require.NoError(t, err)
					t.Cleanup(func() { _ = closer.Close() })
					readMCP()
					standing, err := apiworkspace.Standing(profile)
					require.NoError(t, err)
					prompt, err := os.ReadFile(filepath.Join(input.EphemeralDir, "system-prompt.txt"))
					require.NoError(t, err)
					require.Equal(t, cfg.AppendSystemPrompt+"\n"+standing, string(prompt), "restart restores the installed standing context")
					_, _, err = observer.Setup(t.Context(), assignment, true)
					require.NoError(t, err)
					actualSetup, err := os.ReadFile(setupPath)
					require.NoError(t, err)
					require.Equal(t, originalSetup, actualSetup)
					actualHistory, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
					require.NoError(t, err)
					require.Equal(t, history, actualHistory)
					require.NoFileExists(t, log)

					for _, name := range []string{"mcp.json", "settings.json", "system-prompt.txt"} {
						path := filepath.Join(input.EphemeralDir, name)
						original, err := os.ReadFile(path)
						require.NoError(t, err)
						tampered := []byte("changed native setup")
						require.NoError(t, os.WriteFile(path, tampered, 0o600))
						_, _, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: input.Environment, Workspace: &startupWorkspace{}})
						require.Error(t, err, "startup cannot overwrite tampered present files")
						actual, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, tampered, actual)
						require.NoError(t, os.WriteFile(path, original, 0o600))
					}
				})
			}
		})
	}
}
