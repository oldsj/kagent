package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/driver"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// SetupHook is the native hook the Claude installer uses: the standing context
// is appended to the system prompt.
const SetupHook = "append_system_prompt"

// SetupInstaller applies the fixed hook using the same native configuration path
// as normal execution. It never validates or starts a CLI or binds a continuation.
type SetupInstaller struct {
	Input Input
	Store *continuation.Store
	// Runner is the current driver, including its ephemeral approval broker.
	Runner *driver.ProcessDriver
	// Wrap, if set, adapts the reconfigured driver before it replaces the
	// turn runner (the SDK runner keeps its live session across setup).
	Wrap   func(*driver.ProcessDriver) workspace.Runner
	mu     sync.Mutex
	closed bool
}

// Setup installs input's profile once, or, with observe set, checks that the
// installed setup and its materialized files are unchanged.
func (s *SetupInstaller) Setup(ctx context.Context, input workspace.Preparation, observe bool) (workspace.Runner, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, "", errors.New("Claude setup installer is closed")
	}
	if input.Provider != "claude" {
		return nil, "", errors.New("assigned native provider differs")
	}
	stateDir := filepath.Join(s.Input.DurableDir, ".kagent")
	configured := s.Input
	configured.SetupProfile = input.Profile
	if observe {
		if err := workspace.CheckSetup(s.Input.ConfigJSON, stateDir, input, true); err != nil {
			return nil, "", err
		}
		if err := checkRuntimeSetup(configured, false); err != nil {
			return nil, "", err
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
	runner, err := New(ctx, configured)
	if err != nil {
		return nil, "", err
	}
	if err := workspace.CheckSetup(s.Input.ConfigJSON, stateDir, input, false); err != nil {
		_ = runner.Close()
		return nil, "", err
	}
	if s.Runner != nil {
		_ = s.Runner.Close()
	}
	s.Runner = runner
	if s.Wrap != nil {
		return s.Wrap(runner), SetupHook, nil
	}
	return runner, SetupHook, nil
}

// Close follows the configured driver when preparation replaces it, so the
// executor's shutdown owns every approval broker it constructed.
func (s *SetupInstaller) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.Runner != nil {
		return s.Runner.Close()
	}
	return nil
}

const runtimeSetupFile = "claude-runtime-setup.json"

// runtimeSetup is a private projection that fingerprints the materialized
// files, including the ephemeral authenticated approval bridge. It stores no
// broker token and is separate from the immutable setup record.
type runtimeSetup struct {
	Profile          string `json:"profile"`
	ConfigDigest     string `json:"config_digest"`
	MCPDigest        string `json:"mcp_digest"`
	SetupDigest      string `json:"setup_digest"`
	MCPFile          string `json:"mcp_file"`
	SettingsFile     string `json:"settings_file"`
	SystemPromptFile string `json:"system_prompt_file"`
}

func (r runtimeSetup) files() map[string]string {
	return map[string]string{"mcp.json": r.MCPFile, "settings.json": r.SettingsFile, "system-prompt.txt": r.SystemPromptFile}
}

func runtimeSetupIdentity(input Input) (runtimeSetup, error) {
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(input.ConfigJSON)
	if err != nil {
		return runtimeSetup{}, err
	}
	setupDigest, err := apiworkspace.SetupDigest(input.SetupProfile)
	return runtimeSetup{Profile: input.SetupProfile, ConfigDigest: configDigest, MCPDigest: mcpDigest, SetupDigest: setupDigest}, err
}

func recordRuntimeSetup(input Input, mcpPath, settingsPath, systemPromptPath string) error {
	identity, err := runtimeSetupIdentity(input)
	if err != nil {
		return err
	}
	for path, digest := range map[string]*string{mcpPath: &identity.MCPFile, settingsPath: &identity.SettingsFile, systemPromptPath: &identity.SystemPromptFile} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		*digest = apiworkspace.Digest(data)
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	directory := filepath.Join(input.DurableDir, ".kagent")
	if err := utils.EnsurePrivateDir(directory); err != nil {
		return err
	}
	return utils.ReplacePrivateFile(filepath.Join(directory, runtimeSetupFile), data)
}

// checkRestoredSetup checks a setup a previous process installed, if any,
// before New replaces its materialized files.
func checkRestoredSetup(input Input) error {
	restored, err := workspace.RestoreSetup(input.ConfigJSON, filepath.Join(input.DurableDir, ".kagent"))
	if err != nil || restored == nil {
		return err
	}
	if restored.Profile != input.SetupProfile {
		return errors.New("installed Claude profile differs")
	}
	return checkRuntimeSetup(input, true)
}

// checkRuntimeSetup requires the recorded runtime setup to match input and
// every materialized file to match its recorded digest. With allowMissing, an
// absent file passes, since a restarted Actor loses its ephemeral directory.
func checkRuntimeSetup(input Input, allowMissing bool) error {
	expected, err := runtimeSetupIdentity(input)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(input.DurableDir, ".kagent", runtimeSetupFile))
	if err != nil {
		return fmt.Errorf("read installed Claude runtime setup: %w", err)
	}
	var saved runtimeSetup
	if json.Unmarshal(data, &saved) != nil || saved.Profile != expected.Profile || saved.ConfigDigest != expected.ConfigDigest || saved.MCPDigest != expected.MCPDigest || saved.SetupDigest != expected.SetupDigest {
		return errors.New("installed Claude runtime setup differs")
	}
	for name, digest := range saved.files() {
		if len(digest) != 64 {
			return errors.New("installed Claude runtime setup differs")
		}
		actual, err := os.ReadFile(filepath.Join(input.EphemeralDir, name))
		if allowMissing && os.IsNotExist(err) {
			continue
		}
		if err != nil || apiworkspace.Digest(actual) != digest {
			return fmt.Errorf("installed Claude %s differs", name)
		}
	}
	return nil
}
