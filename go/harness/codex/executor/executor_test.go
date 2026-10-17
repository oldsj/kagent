package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "relative data dir", cfg: Config{ConfigJSON: fmt.Appendf(nil, `{"version":%d,"codex_executable":"codex","model":"m","provider":{"name":"openai"},"max_frame_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`, config.Version), DataDir: "relative/dir"}, wantErr: "absolute path"},
		{name: "malformed config", cfg: Config{ConfigJSON: []byte(`{"version":`), DataDir: t.TempDir()}, wantErr: "decode config"},
		{name: "unknown config field", cfg: Config{ConfigJSON: []byte(`{"nope":1}`), DataDir: t.TempDir()}, wantErr: "decode config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.cfg)
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
	cli := filepath.Join(data, "fake-codex")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' 'codex-cli '+VERSION+''; exit 0; fi\nprintf 'unexpected native turn\\n' >> '" + log + "'\nexit 42\n"
	script = strings.ReplaceAll(script, "'+VERSION+'", config.PinnedCodexVersion)
	require.NoError(t, os.WriteFile(cli, []byte(script), 0700))
	cfg := config.Production("fixture-model", "no turn")
	cfg.CodexExecutable = cli
	cfg.Provider = config.Provider{Name: "openai"}
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(apiworkspace.ReadProxyOrigin), PushProxyOrigin: new(apiworkspace.PushProxyOrigin)}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	source := &startupWorkspace{}
	executor, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: []string{"PATH=" + os.Getenv("PATH")}, Workspace: source})
	require.NoError(t, err)
	require.NotNil(t, executor)

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
			cfg.CodexExecutable = filepath.Join(data, "fake-codex")
			nativeLog := filepath.Join(data, "native-calls")
			require.NoError(t, os.WriteFile(cfg.CodexExecutable, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' 'codex-cli "+cfg.ExpectedCodexVersion+"'; exit 0; fi\nprintf 'native call\\n' >> '"+nativeLog+"'\nexit 42\n"), 0700))
			cfg.Provider = config.Provider{Name: "openai"}
			cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			store, err := continuation.New(filepath.Join(data, "adapter"), "codex", validateThreadID)
			require.NoError(t, err)
			stateDir := filepath.Join(data, ".kagent")
			require.NoError(t, os.MkdirAll(stateDir, 0700))
			installer := &adapter.SetupInstaller{Input: adapter.Input{ConfigJSON: raw, Workspace: filepath.Join(data, "workspace"), DurableDir: data, Environment: nil}, Store: store}
			configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
			require.NoError(t, err)
			setup, err := workspace.SetupDigest(profile)
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
			assignment.SetupDigest = strings.Repeat("0", 64)
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			assignment.SetupDigest = setup
			path := filepath.Join(data, "codex", "config.toml")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("changed native setup"), 0600))
			_, _, err = installer.Setup(t.Context(), assignment, true)
			require.Error(t, err)
			_, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data})
			require.ErrorContains(t, err, "installed Codex configuration differs")
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "changed native setup", string(actual), "restart cannot repair unqualified setup")
			require.NoError(t, os.WriteFile(path, original, 0600))
			require.NoError(t, store.Bind("thread-fixture"))
			history, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
			require.NoError(t, err)
			setupPath := filepath.Join(stateDir, "native-setup.json")
			originalSetup, err := os.ReadFile(setupPath)
			require.NoError(t, err)
			_, err = New(t.Context(), Config{ConfigJSON: raw, DataDir: data})
			require.NoError(t, err)
			actualHistory, err := os.ReadFile(filepath.Join(data, "adapter", "state.json"))
			require.NoError(t, err)
			require.Equal(t, history, actualHistory, "restart preserves the owner's native thread")
			actualSetup, err := os.ReadFile(setupPath)
			require.NoError(t, err)
			require.Equal(t, originalSetup, actualSetup)
			require.NoFileExists(t, nativeLog)
			_, _, err = installer.Setup(t.Context(), assignment, false)
			require.Error(t, err)
		})
	}
}

type forkConversation struct{ fresh bool }

func (forkConversation) SessionID() (string, error) { return "fork-session", nil }
func (c forkConversation) FreshConversation(context.Context) (bool, error) {
	return c.fresh, nil
}

// A fresh fork's first turn drops the restored Codex conversation, and only
// that: configuration and the workspace stay.
func TestFreshForkDropsCodexConversation(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprint(fresh), func(t *testing.T) {
			data := t.TempDir()
			cli := filepath.Join(data, "fake-codex")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' 'codex-cli " + config.PinnedCodexVersion + "'; exit 0; fi\nexit 42\n"
			require.NoError(t, os.WriteFile(cli, []byte(script), 0700))
			cfg := config.Production("fixture-model", "no turn")
			cfg.CodexExecutable = cli
			cfg.Provider = config.Provider{Name: "openai"}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)

			const bound = "thread-1"
			restored, err := continuation.New(filepath.Join(data, "adapter"), "codex", validateThreadID)
			require.NoError(t, err)
			require.NoError(t, restored.Bind(bound))
			conversation := []string{"sessions/2026/rollout-thread-1.jsonl", "archived_sessions/old.jsonl", "history.jsonl", "session_index.jsonl"}
			kept := []string{"codex/config.toml", "workspace/main.go", "cache/modules/m"}
			for _, name := range kept {
				write(t, filepath.Join(data, name))
			}
			for _, name := range conversation {
				write(t, filepath.Join(data, "codex", name))
			}

			executor, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: []string{"PATH=" + os.Getenv("PATH")}, Conversation: forkConversation{fresh: fresh}})
			require.NoError(t, err)
			message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
			message.TaskID, message.ContextID = "task-1", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			for range executor.Execute(t.Context(), &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: message.ContextID, Message: message}) {
			}

			_, started, err := restored.Load()
			require.NoError(t, err)
			require.Equal(t, !fresh, started, "the binding survives only a default fork")
			for _, name := range conversation {
				if fresh {
					require.NoFileExists(t, filepath.Join(data, "codex", name))
				} else {
					require.FileExists(t, filepath.Join(data, "codex", name))
				}
			}
			for _, name := range kept {
				require.FileExists(t, filepath.Join(data, name))
			}
		})
	}
}

func write(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))
}
