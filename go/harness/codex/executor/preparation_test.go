package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

func TestPreparationSetupHasNoNativeHistory(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			data := t.TempDir()
			cfg := config.Production("fixture-model", "original instructions")
			cfg.CodexExecutable = filepath.Join(data, "fake-codex")
			nativeLog := filepath.Join(data, "native-calls")
			require.NoError(t, os.WriteFile(cfg.CodexExecutable, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' 'codex-cli "+cfg.ExpectedCodexVersion+"'; exit 0; fi\nprintf 'native call\\n' >> '"+nativeLog+"'\nexit 42\n"), 0o700))
			cfg.Provider = config.Provider{Name: "openai"}
			cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
			cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			store, err := continuation.New(filepath.Join(data, "adapter"), "codex", validateThreadID)
			require.NoError(t, err)
			stateDir := filepath.Join(data, ".kagent")
			require.NoError(t, os.MkdirAll(stateDir, 0o700))
			installer := &adapter.SetupInstaller{Input: adapter.Input{ConfigJSON: raw, Workspace: filepath.Join(data, "workspace"), DurableDir: data}, Store: store}
			configDigest, mcpDigest, err := apiworkspace.ConfigDigests(raw)
			require.NoError(t, err)
			setup, err := apiworkspace.SetupDigest(profile)
			require.NoError(t, err)
			assignment := workspace.Preparation{Provider: "codex", Profile: profile, SetupDigest: setup, ConfigDigest: configDigest, MCPDigest: mcpDigest}

			runner, hook, err := installer.Setup(t.Context(), assignment, false)
			require.NoError(t, err)
			require.NotNil(t, runner)
			require.Equal(t, "developer_instruction", hook)
			_, started, err := store.Load()
			require.NoError(t, err)
			require.False(t, started)

			_, hook, err = installer.Setup(t.Context(), assignment, true)
			require.NoError(t, err)
			require.Equal(t, "developer_instruction", hook)
			wrongProvider := assignment
			wrongProvider.Provider = "claude"
			_, _, err = installer.Setup(t.Context(), wrongProvider, true)
			require.Error(t, err)
			assignment.SetupDigest = strings.Repeat("0", 64)
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			assignment.SetupDigest = setup

			path := filepath.Join(data, "codex", "config.toml")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("changed native setup"), 0o600))
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			_, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Workspace: &startupWorkspace{}})
			require.ErrorContains(t, err, "installed Codex configuration differs")
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "changed native setup", string(actual), "restart cannot repair unqualified setup")
			require.NoError(t, os.WriteFile(path, original, 0o600))

			require.NoError(t, store.Bind("thread-fixture"))
			history, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
			require.NoError(t, err)
			setupPath := filepath.Join(stateDir, "native-setup.json")
			originalSetup, err := os.ReadFile(setupPath)
			require.NoError(t, err)
			_, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Workspace: &startupWorkspace{}})
			require.NoError(t, err)
			actualHistory, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
			require.NoError(t, err)
			require.Equal(t, history, actualHistory, "restart preserves the owner's native thread")
			actualSetup, err := os.ReadFile(setupPath)
			require.NoError(t, err)
			require.Equal(t, originalSetup, actualSetup)
			require.NoFileExists(t, nativeLog)
			_, _, err = installer.Setup(t.Context(), assignment, false)
			require.Error(t, err, "setup never applies over native history")
		})
	}
}
