// Package executor builds the Codex Harness A2A executor for kagent-codex
// and for binaries that embed the harness elsewhere.
package executor

import (
	"context"
	"fmt"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/adapter"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// Config is the input to New.
type Config struct {
	// ConfigJSON is the compiler-owned KAGENT_CONFIG_JSON document.
	ConfigJSON []byte
	// DataDir is the absolute durable directory; it is created if missing.
	DataDir string
	// Environment is the process environment passed to the Codex CLI.
	Environment []string
	// Workspace reads the Session's workspace request. It is required when the
	// configuration enables Git, and ignored otherwise.
	Workspace workspace.Source
}

// New validates the configuration and Codex installation, then returns the executor.
func New(ctx context.Context, cfg Config) (a2asrv.AgentExecutor, error) {
	restored, err := workspace.RestoreSetup(cfg.ConfigJSON, cfg.DataDir+"/.kagent")
	if err != nil {
		return nil, err
	}
	input := adapter.Input{
		ConfigJSON: cfg.ConfigJSON, Workspace: cfg.DataDir + "/workspace", DurableDir: cfg.DataDir, Environment: cfg.Environment,
	}
	if restored != nil {
		input.SetupProfile, restored.Provider = restored.Profile, "codex"
		// Verify before normal startup materialization can replace a changed file.
		if _, _, err := (&adapter.SetupInstaller{Input: input}).Setup(ctx, *restored, true); err != nil {
			return nil, err
		}
	}
	runner, err := adapter.New(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("configure Codex Harness: %w", err)
	}
	validateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := runner.Validate(validateCtx); err != nil {
		return nil, err
	}
	store, err := continuation.New(cfg.DataDir+"/adapter", "codex", validateThreadID)
	if err != nil {
		return nil, err
	}
	// Read here rather than taken as a Config field: the identity travels in the
	// compiled configuration this function already has, and an embedder that
	// forgot to pass it would emit spans no consumer could attribute to a
	// harness, which is a silence rather than an error.
	parsed, err := config.Parse(cfg.ConfigJSON)
	if err != nil {
		return nil, err
	}
	var turns runtimea2a.Runner = runner
	if parsed.Git != nil {
		if cfg.Workspace == nil {
			return nil, fmt.Errorf("configuration enables git but no workspace source was provided")
		}
		bootstrap, bootstrapErr := workspace.New(runner, workspace.Config{
			Dir: cfg.DataDir + "/workspace", StateDir: cfg.DataDir + "/.kagent", Policy: *parsed.Git, Source: cfg.Workspace, Environment: cfg.Environment,
		})
		if bootstrapErr != nil {
			return nil, bootstrapErr
		}
		input.SetupProfile = ""
		bootstrap.ServePreparations(ctx, &adapter.SetupInstaller{Input: input, Store: store})
		turns = bootstrap
	}
	executor, err := runtimea2a.New(turns, store, parsed.RuntimeTelemetry)
	if err != nil {
		return nil, err
	}
	return executor, nil
}

func validateThreadID(id string) error {
	if id == "" || len(id) > 256 {
		return fmt.Errorf("invalid Codex thread ID length")
	}
	for _, character := range id {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("invalid Codex thread ID")
		}
	}
	return nil
}
