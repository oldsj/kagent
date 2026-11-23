package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

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
	// Restart marks the check a harness process makes of a workspace an earlier
	// process prepared, before New rewrites config.toml. That earlier process
	// may have been another harness version, or had another environment, and
	// rendered other bytes; so a restart checks that the file is unchanged
	// since a harness wrote it, not that this harness would render the same.
	Restart bool
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
		check := checkInstalledConfig
		if s.Restart {
			check = checkWrittenConfig
		}
		if err := check(s.Input); err != nil {
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

// checkInstalledConfig reports whether config.toml still holds exactly the
// bytes New renders for input. Every turn of a prepared workspace checks it,
// so anything that rewrites the file (Codex itself included) fails the next
// turn with "Workspace preparation is incomplete."
func checkInstalledConfig(input Input) error {
	cfg, err := config.Parse(input.ConfigJSON)
	if err != nil {
		return err
	}
	cfg, _ = withNativeTelemetry(cfg, input)
	expected, err := renderInputConfig(cfg, input)
	actual, readErr := os.ReadFile(filepath.Join(input.DurableDir, "codex", "config.toml"))
	if err != nil || readErr != nil || !bytes.Equal(expected, actual) {
		return errors.New("installed Codex configuration differs")
	}
	return nil
}

// configRecordPath holds the SHA-256 of the config.toml New last wrote.
func configRecordPath(input Input) string {
	return filepath.Join(input.DurableDir, ".kagent", "codex-config.sha256")
}

func configDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

// checkWrittenConfig reports whether config.toml still holds the bytes a
// harness last wrote, by the digest New recorded with them. A workspace whose
// harness recorded none is checked against this harness's render, as before.
func checkWrittenConfig(input Input) error {
	record, err := os.ReadFile(configRecordPath(input))
	if errors.Is(err, os.ErrNotExist) {
		return checkInstalledConfig(input)
	}
	actual, readErr := os.ReadFile(filepath.Join(input.DurableDir, "codex", "config.toml"))
	if err != nil || readErr != nil || strings.TrimSpace(string(record)) != configDigest(actual) {
		return errors.New("installed Codex configuration differs")
	}
	return nil
}
