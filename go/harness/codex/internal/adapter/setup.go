package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// SetupHook is the native hook the Codex installer uses: the standing context
// is appended to the developer instruction.
const SetupHook = "developer_instruction"

// SetupInstaller applies the fixed hook using the same native configuration path
// as normal execution. It never validates or starts a CLI or binds a continuation.
type SetupInstaller struct {
	Input Input
	Store *continuation.Store
}

// Setup installs input's profile once, or, with observe set, checks that the
// installed setup and the generated config.toml are unchanged.
func (s *SetupInstaller) Setup(ctx context.Context, input workspace.Preparation, observe bool) (workspace.Runner, string, error) {
	if input.Provider != "codex" {
		return nil, "", errors.New("assigned native provider differs")
	}
	stateDir := filepath.Join(s.Input.DurableDir, ".kagent")
	if observe {
		if err := workspace.CheckSetup(s.Input.ConfigJSON, stateDir, input, true); err != nil {
			return nil, "", err
		}
		cfg, err := config.Parse(s.Input.ConfigJSON)
		if err != nil {
			return nil, "", err
		}
		cfg, _ = withNativeTelemetry(cfg, s.Input)
		expected, err := renderInputConfig(cfg, s.Input)
		actual, readErr := os.ReadFile(filepath.Join(s.Input.DurableDir, "codex", "config.toml"))
		if err != nil || readErr != nil || !bytes.Equal(expected, actual) {
			return nil, "", errors.New("installed Codex configuration differs")
		}
		return nil, SetupHook, nil
	}
	if s.Store == nil {
		return nil, "", errors.New("continuation store is required")
	}
	if _, started, err := s.Store.Load(); err != nil || started {
		return nil, "", errors.New("native history already exists or is malformed")
	}
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(s.Input.ConfigJSON)
	if err != nil || configDigest != input.ConfigDigest || mcpDigest != input.MCPDigest {
		return nil, "", errors.New("assigned compiler configuration differs")
	}
	configured := s.Input
	configured.SetupProfile = input.Profile
	runner, err := New(ctx, configured)
	if err != nil {
		return nil, "", err
	}
	if err := workspace.CheckSetup(s.Input.ConfigJSON, stateDir, input, false); err != nil {
		return nil, "", err
	}
	return runner, SetupHook, nil
}
