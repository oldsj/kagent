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
	"github.com/kagent-dev/kagent/go/harness/claude/config"
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

type forkConversation struct{ fresh bool }

func (forkConversation) SessionID() (string, error) { return "fork-session", nil }
func (c forkConversation) FreshConversation(context.Context) (bool, error) {
	return c.fresh, nil
}

// A fresh fork's first turn drops the restored Claude conversation, and only
// that: settings, packages and the workspace stay.
func TestFreshForkDropsClaudeConversation(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprint(fresh), func(t *testing.T) {
			data := t.TempDir()
			cli := filepath.Join(data, "fake-claude")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + config.PinnedClaudeVersion + " (Claude Code)'; exit 0; fi\nexit 42\n"
			require.NoError(t, os.WriteFile(cli, []byte(script), 0700))
			cfg := config.Production("fixture-model", "no turn")
			cfg.ClaudeExecutable = cli
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)

			const bound = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			restored, err := continuation.New(filepath.Join(data, "adapter"), "claude", validateSessionID)
			require.NoError(t, err)
			require.NoError(t, restored.Bind(bound))
			conversation := []string{
				"claude/projects/work/" + bound + ".jsonl", "claude/todos/t.json", "claude/history.jsonl", "claude/sessions/s.json", "claude/plans/p.md",
				// The SDK runner's user profile keeps its transcripts in the unpacked home.
				"sdk-runner/home/.claude/projects/work/" + bound + ".jsonl", "sdk-runner/home/.claude/history.jsonl",
			}
			kept := []string{
				"claude/settings.json", "claude/packages/p/index.js", "workspace/main.go", "cache/modules/m",
				"sdk-runner/home/.claude/settings.json", "sdk-runner/home/.claude/skills/s/SKILL.md",
			}
			for _, name := range kept {
				write(t, filepath.Join(data, name))
			}
			for _, name := range conversation {
				write(t, filepath.Join(data, name))
			}

			executor, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: data, Environment: []string{"PATH=" + os.Getenv("PATH")}, Conversation: forkConversation{fresh: fresh}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = closer.Close() })
			message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
			message.TaskID, message.ContextID = "task-1", bound
			for range executor.Execute(t.Context(), &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: bound, Message: message}) {
			}

			_, started, err := restored.Load()
			require.NoError(t, err)
			require.Equal(t, !fresh, started, "the binding survives only a default fork")
			for _, name := range conversation {
				if fresh {
					require.NoFileExists(t, filepath.Join(data, name))
				} else {
					require.FileExists(t, filepath.Join(data, name))
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
