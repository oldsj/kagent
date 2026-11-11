// Package executor builds the Claude Harness A2A executor for kagent-claude
// and for binaries that embed the harness elsewhere.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/adapter"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/driver"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// Config is the input to New.
type Config struct {
	// ConfigJSON is the compiler-owned bootstrap configuration document.
	ConfigJSON []byte
	// DataDir is the absolute durable directory; it is created if missing.
	DataDir string
	// Environment is the process environment passed to the Claude CLI.
	Environment []string
	// ClientAddr is where the SDK runner serves its clients when the
	// environment selects that driver. Empty serves none, as for a check run.
	ClientAddr string
	// Activity receives the SDK runner's busy/idle changes. Nil reports none.
	Activity sdkrunner.ActivityReporter
	// Workspace reads the Session's workspace request. It is required when the
	// configuration enables Git, and ignored otherwise.
	Workspace workspace.Source
	// Conversation says whether the Session is a fork that starts without the
	// native conversation in its restored data. Nil leaves that data as it is,
	// which only a configuration check and an embedder without a control plane
	// should do.
	Conversation continuation.FreshSource
}

// New validates the configuration and Claude installation, then returns the
// executor and the resource to close on shutdown.
func New(ctx context.Context, cfg Config) (a2asrv.AgentExecutor, io.Closer, error) {
	stateDir := cfg.DataDir + "/.kagent"
	// A setup installed by an earlier process is restored, so its standing
	// context survives a restart without another preparation.
	restored, err := workspace.RestoreSetup(cfg.ConfigJSON, stateDir)
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
		input.SetupProfile = restored.Profile
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
	if cfg.Conversation != nil {
		store.RequireFresh(continuation.Fresh{
			Source: cfg.Conversation, MarkerDir: cfg.DataDir + "/.kagent",
			Clear: claudeConversationPaths(cfg.DataDir),
		})
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
	mode, err := selectedDriver(cfg.Environment)
	if err != nil {
		_ = runner.Close()
		return nil, nil, err
	}
	var turns runtimea2a.Runner = runner
	// runnerCloser owns the current driver. Preparation can replace it, so
	// with Git it becomes the setup installer, which closes whichever is current.
	var runnerCloser io.Closer = runner
	var sdk *sdkWiring
	if mode == modeSDK {
		bridge := imageSDKRunner
		if parsed.SDKRunner != nil {
			bridge = *parsed.SDKRunner
		}
		if sdk, err = newSDKWiring(ctx, cfg, bridge, runner, store, parsed.InterruptGrace()); err != nil {
			_ = runner.Close()
			return nil, nil, err
		}
		turns = sdk.driver(runner)
	}
	// The checkout wraps whichever driver serves turns, so both establish the
	// workspace before the first one.
	if parsed.Git != nil {
		if cfg.Workspace == nil {
			_ = runner.Close()
			return nil, nil, fmt.Errorf("configuration enables git but no workspace source was provided")
		}
		bootstrap, err := workspace.New(turns, workspace.Config{
			Dir: cfg.DataDir + "/workspace", StateDir: stateDir, Policy: *parsed.Git, Source: cfg.Workspace, Environment: cfg.Environment,
		})
		if err != nil {
			_ = runner.Close()
			return nil, nil, err
		}
		// The installer configures each assigned profile itself.
		input.SetupProfile = ""
		installer := &adapter.SetupInstaller{Input: input, Store: store, Runner: runner}
		if sdk != nil {
			installer.Wrap = func(d *driver.ProcessDriver) workspace.Runner { return sdk.driver(d) }
			// A client attaching to the SDK runner starts native work outside a
			// turn, so it passes the same admission and checkout first.
			sdk.prepare = func(ctx context.Context) error {
				if failure := bootstrap.Ready(ctx); failure != "" {
					return errors.New(failure)
				}
				return nil
			}
		}
		bootstrap.ServePreparations(ctx, installer)
		turns, runnerCloser = bootstrap, installer
	}
	closer := closers{runnerCloser}
	if sdk != nil {
		served, err := sdk.serve(ctx, cfg.ClientAddr)
		if err != nil {
			_ = closer.Close()
			return nil, nil, err
		}
		// Stop the client port and the bridge before the driver they use.
		closer = closers{served, runnerCloser}
	}
	executor, err := runtimea2a.New(turns, store, parsed.RuntimeTelemetry, config.EffortLevels...)
	if err != nil {
		_ = closer.Close()
		return nil, nil, err
	}
	return executor, closer, nil
}

// claudeConversationPaths are the transcript and per-conversation files Claude
// keeps under its configuration directory. Settings and credentials are not
// among them. The process driver and the SDK runner's managed profile use
// <data>/claude; the SDK runner's user profile uses the .claude directory of
// the home it unpacked.
func claudeConversationPaths(dataDir string) []string {
	var paths []string
	for _, dir := range []string{filepath.Join(dataDir, "claude"), filepath.Join(dataDir, "sdk-runner", "home", ".claude")} {
		for _, name := range []string{"projects", "todos", "shell-snapshots", "file-history", "session-env", "sessions", "plans", "history.jsonl"} {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths
}

func validateSessionID(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("invalid Claude session ID: %w", err)
	}
	return nil
}
