package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
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
