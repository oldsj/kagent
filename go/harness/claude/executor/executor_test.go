package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/stretchr/testify/require"
)

func TestValidateSessionID(t *testing.T) {
	if err := validateSessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"); err != nil {
		t.Fatalf("validateSessionID() error = %v", err)
	}
	if err := validateSessionID("not-a-session"); err == nil {
		t.Fatal("validateSessionID() accepted an invalid UUID")
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "relative data dir", cfg: Config{ConfigJSON: fmt.Appendf(nil, `{"version":%d,"claude_executable":"claude","model":"m","max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`, config.Version), DataDir: "relative/dir"}, wantErr: "absolute path"},
		{name: "malformed config", cfg: Config{ConfigJSON: []byte(`{"version":`), DataDir: t.TempDir()}, wantErr: "decode config"},
		{name: "unknown config field", cfg: Config{ConfigJSON: []byte(`{"nope":1}`), DataDir: t.TempDir()}, wantErr: "decode config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := New(context.Background(), tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New() err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

type startupWorkspace struct{ calls int }

func (s *startupWorkspace) Workspace(context.Context) (*workspace.Request, error) {
	s.calls++
	return nil, fmt.Errorf("Git capability deliberately absent")
}
func TestProxyExecutorStartupIsLazy(t *testing.T) {
	data := t.TempDir()
	log := filepath.Join(data, "native-calls")
	cli := filepath.Join(data, "fake-claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' ''+VERSION+' (Claude Code)'; exit 0; fi\nprintf 'unexpected native turn\\n' >> '" + log + "'\nexit 42\n"
	script = strings.ReplaceAll(script, "'+VERSION+'", config.PinnedClaudeVersion)
	require.NoError(t, os.WriteFile(cli, []byte(script), 0700))
	cfg := config.Production("fixture-model", "no turn")
	cfg.ClaudeExecutable = cli

	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	source := &startupWorkspace{}
	executor, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: []string{"PATH=" + os.Getenv("PATH")}, Workspace: source})
	require.NoError(t, err)
	require.NotNil(t, executor)
	require.NoError(t, closer.Close())
	require.Zero(t, source.calls)
	for _, name := range []string{"native-calls", "workspace/.git", ".kagent/workspace-bootstrap.started", ".kagent/workspace-bootstrap.done"} {
		_, err := os.Stat(filepath.Join(data, name))
		require.True(t, os.IsNotExist(err), name)
	}
}

func TestPreparationSetupHasNoNativeHistory(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			data := t.TempDir()
			cfg := config.Production("fixture-model", "original instructions")

			cfg.MCPServers = map[string]config.MCPServer{"mainloop": {Type: "http", URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			store, err := continuation.New(filepath.Join(data, "adapter"), "claude", validateSessionID)
			require.NoError(t, err)
			stateDir := filepath.Join(data, ".kagent")
			require.NoError(t, os.MkdirAll(stateDir, 0700))
			installer := &adapter.SetupInstaller{Input: adapter.Input{ConfigJSON: raw, Workspace: filepath.Join(data, "workspace"), DurableDir: data, EphemeralDir: filepath.Join(data, "ephemeral")}, Store: store}
			configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
			require.NoError(t, err)
			setup, err := workspace.SetupDigest(profile)
			require.NoError(t, err)
			assignment := workspace.Preparation{Provider: "claude", Profile: profile, SetupDigest: setup, ConfigDigest: configDigest, MCPDigest: mcpDigest}
			runner, hook, err := installer.Setup(t.Context(), assignment, false)
			require.NoError(t, err)
			require.NotNil(t, runner)
			require.Equal(t, "append_system_prompt", hook)
			standing, err := workspace.Standing(profile)
			require.NoError(t, err)
			args := runner.(interface{ Args(runtime.Turn) []string }).Args(runtime.Turn{Prompt: "owner instruction"})
			require.Contains(t, args, cfg.AppendSystemPrompt+"\n"+standing)
			t.Cleanup(func() { _ = runner.(interface{ Close() error }).Close() })
			_, started, err := store.Load()
			require.NoError(t, err)
			require.False(t, started)
			_, hook, err = installer.Setup(t.Context(), assignment, true)
			require.NoError(t, err)
			require.Equal(t, "append_system_prompt", hook)
			assignment.SetupDigest = strings.Repeat("0", 64)
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			assignment.SetupDigest = setup
			path := filepath.Join(data, "ephemeral", "mcp.json")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("changed native setup"), 0600))
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			require.NoError(t, os.WriteFile(path, original, 0600))
			require.NoError(t, store.Bind("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
			_, _, err = installer.Setup(t.Context(), assignment, false)
			require.Error(t, err)
		})
	}
}

func TestPreparationRuntimeRecoveryPreservesApprovalAndHistory(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			for _, protected := range []bool{false, true} {
				t.Run(fmt.Sprintf("approval=%t", protected), func(t *testing.T) {
					data := t.TempDir()
					cfg := config.Production("fixture-model", "original instructions")
					cfg.ClaudeExecutable = filepath.Join(data, "fake-claude")
					log := filepath.Join(data, "native-calls")
					require.NoError(t, os.WriteFile(cfg.ClaudeExecutable, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '"+cfg.ExpectedClaudeVersion+" (Claude Code)'; exit 0; fi\nprintf 'native call\\n' >> '"+log+"'\nexit 42\n"), 0700))
					cfg.MCPServers = map[string]config.MCPServer{"mainloop": {Type: "http", URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp", Headers: map[string]string{"Authorization": "${MAINLOOP_MCP_AUTHORIZATION}"}, RequireApproval: protected}}
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
					configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
					require.NoError(t, err)
					setup, err := workspace.SetupDigest(profile)
					require.NoError(t, err)
					assignment := workspace.Preparation{Provider: "claude", Profile: profile, SetupDigest: setup, ConfigDigest: configDigest, MCPDigest: mcpDigest}
					_, _, err = installer.Setup(t.Context(), assignment, false)
					require.NoError(t, err)
					servers := readMCP()
					var bridge string
					if protected {
						bridge = servers["kagent_hitl"].URL
						require.NotEmpty(t, servers["kagent_hitl"].Headers["Authorization"])
						require.NotEqual(t, oldBridge, bridge)
						_, err := http.Get(oldBridge)
						require.Error(t, err, "replaced startup broker must close")
						response, err := http.Get(bridge)
						require.NoError(t, err)
						require.Equal(t, http.StatusUnauthorized, response.StatusCode)
						require.NoError(t, response.Body.Close())
						settings, err := os.ReadFile(filepath.Join(input.EphemeralDir, "settings.json"))
						require.NoError(t, err)
						require.Contains(t, string(settings), "mcp__mainloop__*")
					}
					setupPath := filepath.Join(data, ".kagent", "native-setup.json")
					originalSetup, err := os.ReadFile(setupPath)
					require.NoError(t, err)
					// Existing continuation is retained across startup reconstruction.
					require.NoError(t, store.Bind("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
					history, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
					require.NoError(t, err)
					require.NoError(t, installer.Close())
					if protected {
						_, err := http.Get(bridge)
						require.Error(t, err, "executor closer must own configured broker")
					}
					for _, name := range []string{"mcp.json", "settings.json"} {
						if name == "settings.json" && !protected {
							continue
						}
						path := filepath.Join(input.EphemeralDir, name)
						require.NoError(t, os.Remove(path))
					}
					observer := &adapter.SetupInstaller{Input: input, Store: store}
					_, _, err = observer.Setup(t.Context(), assignment, true)
					require.Error(t, err, "read-only observation cannot reinstall absent files")
					require.NoFileExists(t, filepath.Join(input.EphemeralDir, "mcp.json"))
					_, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: input.Environment})
					require.NoError(t, err)
					t.Cleanup(func() { _ = closer.Close() })
					readMCP()
					_, _, err = observer.Setup(t.Context(), assignment, true)
					require.NoError(t, err)
					actualSetup, err := os.ReadFile(setupPath)
					require.NoError(t, err)
					require.Equal(t, originalSetup, actualSetup)
					actualHistory, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
					require.NoError(t, err)
					require.Equal(t, history, actualHistory)
					require.NoFileExists(t, log)
					for _, name := range []string{"mcp.json", "settings.json"} {
						if name == "settings.json" && !protected {
							continue
						}
						path := filepath.Join(input.EphemeralDir, name)
						original, err := os.ReadFile(path)
						require.NoError(t, err)
						tampered := []byte("changed native setup")
						require.NoError(t, os.WriteFile(path, tampered, 0600))
						_, _, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: input.Environment})
						require.Error(t, err, "startup cannot overwrite tampered present files")
						actual, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, tampered, actual)
						require.NoError(t, os.WriteFile(path, original, 0600))
					}
				})
			}
		})
	}
}
