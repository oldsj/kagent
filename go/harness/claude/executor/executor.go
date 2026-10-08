// Package executor builds the Claude Harness A2A executor for kagent-claude
// and for binaries that embed the harness elsewhere.
package executor

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/adapter"
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
	// Environment is the process environment passed to the Claude CLI.
	Environment []string
	// Workspace reads the Session's workspace request. It is required when the
	// configuration enables Git, and ignored otherwise.
	Workspace workspace.Source
}

// New validates the configuration and Claude installation, then returns the
// executor and the resource to close on shutdown.
func New(ctx context.Context, cfg Config) (a2asrv.AgentExecutor, io.Closer, error) {
	restored, err := workspace.RestoreSetup(cfg.ConfigJSON, cfg.DataDir+"/.kagent")
	if err != nil {
		return nil, nil, err
	}
	input := adapter.Input{
		ConfigJSON: cfg.ConfigJSON,
		Workspace:  cfg.DataDir + "/workspace", DurableDir: cfg.DataDir,
		EphemeralDir: "/tmp/kagent-claude",
		Environment:  cfg.Environment,
	}
	if restored != nil {
		input.SetupProfile, restored.Provider = restored.Profile, "claude"
	}
	runner, err := adapter.New(ctx, input)
	if err != nil {
		return nil, nil, fmt.Errorf("configure Claude Harness: %w", err)
	}
	validateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := runner.Validate(validateCtx); err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	store, err := continuation.New(cfg.DataDir+"/adapter", "claude", validateSessionID)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	// Read here rather than taken as a Config field: the identity travels in the
	// compiled configuration this function already has, and an embedder that
	// forgot to pass it would emit spans no consumer could attribute to a
	// harness, which is a silence rather than an error.
	parsed, err := config.Parse(cfg.ConfigJSON)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	var turns runtimea2a.Runner = runner
	var closer io.Closer = runner
	if parsed.Git != nil {
		if cfg.Workspace == nil {
			_ = runner.Close()
			return nil, nil, fmt.Errorf("configuration enables git but no workspace source was provided")
		}
		bootstrap, bootstrapErr := workspace.New(runner, workspace.Config{
			Dir: cfg.DataDir + "/workspace", StateDir: cfg.DataDir + "/.kagent", Policy: *parsed.Git, Source: cfg.Workspace, Environment: cfg.Environment,
		})
		if bootstrapErr != nil {
			_ = runner.Close()
			return nil, nil, bootstrapErr
		}
		input.SetupProfile = ""
		installer := &adapter.SetupInstaller{Input: input, Store: store, Runner: runner}
		bootstrap.ServePreparations(ctx, installer)
		closer = installer
		turns = bootstrap
	}
	executor, err := runtimea2a.New(turns, store, parsed.RuntimeTelemetry)
	if err != nil {
		_ = closer.Close()
		return nil, nil, err
	}
	return executor, closer, nil
}

func validateSessionID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("invalid Claude session ID: %w", err)
	}
	return nil
}
