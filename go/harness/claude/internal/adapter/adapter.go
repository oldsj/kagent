// Package adapter constructs the Claude runtime from compiler-owned
// configuration and Actor-owned paths.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kagent-dev/kagent/go/core/pkg/agentplugins"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/driver"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/kagent-dev/kagent/go/pkg/telemetry"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const approvalMCPServerName = "kagent_hitl"

// Input contains compiler output and Actor-owned locations used to construct
// the Claude driver.
type Input struct {
	SetupProfile string
	ConfigJSON   []byte
	Workspace    string
	DurableDir   string
	EphemeralDir string
	Environment  []string
}

// New validates and materializes Claude-owned state, then constructs its driver.
func New(ctx context.Context, input Input) (*driver.ProcessDriver, error) {
	cfg, err := config.Parse(input.ConfigJSON)
	if err != nil {
		return nil, err
	}
	if input.SetupProfile != "" {
		standing, err := workspace.Standing(input.SetupProfile)
		if err != nil {
			return nil, err
		}
		cfg.AppendSystemPrompt += "\n" + standing
		// Startup may recreate missing ephemeral files, but must check any present
		// files before normal materialization can replace them.
		restored, err := workspace.RestoreSetup(input.ConfigJSON, filepath.Join(input.DurableDir, ".kagent"))
		if err != nil {
			return nil, err
		}
		if restored != nil {
			if restored.Profile != input.SetupProfile {
				return nil, fmt.Errorf("installed Claude profile differs")
			}
			if err := checkRuntimeSetup(input, true); err != nil {
				return nil, err
			}
		}
	}
	agentsJSON, err := cfg.AgentsJSON()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(input.Workspace) || !filepath.IsAbs(input.DurableDir) || !filepath.IsAbs(input.EphemeralDir) {
		return nil, fmt.Errorf("workspace, durable, and ephemeral directories must be absolute paths")
	}
	claudeDir := filepath.Join(input.DurableDir, "claude")
	var skillRoot string
	if cfg.SkillResources != nil {
		skillRoot = filepath.Join(input.DurableDir, "generated", "claude")
	}
	for _, directory := range []struct{ name, path string }{
		{name: "workspace", path: input.Workspace},
		{name: "Claude state", path: claudeDir},
	} {
		if err := utils.EnsurePrivateDir(directory.path); err != nil {
			return nil, fmt.Errorf("prepare %s directory: %w", directory.name, err)
		}
	}
	var pluginDirs []string
	if cfg.SkillResources != nil {
		skillsDir := filepath.Join(skillRoot, ".claude", "skills")
		if err := utils.EnsurePrivateDir(skillsDir); err != nil {
			return nil, fmt.Errorf("prepare generated Claude skills directory: %w", err)
		}
		materialized, err := agentplugins.Materialize(ctx, *cfg.SkillResources, agentplugins.Paths{
			Packages: filepath.Join(claudeDir, "packages"),
			Skills:   skillsDir,
		})
		if err != nil {
			return nil, fmt.Errorf("materialize Claude skills: %w", err)
		}
		pluginDirs = materialized.ClaudeFormatPluginRoots()
	}
	environment := setEnvironment(input.Environment, config.ClaudeConfigDirEnvName, claudeDir)
	// The native runtime inherits the compiled identity through the standard
	// resource variable, so no user-supplied marker is required.
	environment, awaitTelemetry := nativeTelemetryEnvironment(telemetry.WithDefaults(tracing.ResourceEnvironment(environment, cfg.RuntimeTelemetry.ChildResource())), cfg.RuntimeTelemetry)
	// The image and compiler pin an exact Claude version. Prevent both automatic
	// and manual update paths from changing that runtime after validation.
	environment = setEnvironment(environment, config.DisableUpdatesEnvName, "1")
	environment, err = materializeGoogleCredentials(environment, input.EphemeralDir)
	if err != nil {
		return nil, err
	}
	protectedServers := approvalServerNames(cfg.MCPServers)
	var approvalBroker *driver.ApprovalBroker
	var settingsPath string
	var permissionPromptTool string
	if len(protectedServers) != 0 {
		if _, exists := cfg.MCPServers[approvalMCPServerName]; exists {
			return nil, fmt.Errorf("Claude MCP server name %q is reserved for human approval", approvalMCPServerName)
		}
		if err := utils.EnsurePrivateDir(input.EphemeralDir); err != nil {
			return nil, fmt.Errorf("prepare ephemeral Claude settings directory: %w", err)
		}
		approvalBroker, err = driver.NewApprovalBroker(protectedServers, cfg.MaxEventBytes)
		if err != nil {
			return nil, fmt.Errorf("start Claude approval broker: %w", err)
		}
		settingsJSON, settingsErr := approvalBroker.SettingsJSON()
		if settingsErr != nil {
			_ = approvalBroker.Close()
			return nil, settingsErr
		}
		settingsPath = filepath.Join(input.EphemeralDir, "settings.json")
		if err := utils.ReplacePrivateFile(settingsPath, settingsJSON); err != nil {
			_ = approvalBroker.Close()
			return nil, fmt.Errorf("materialize Claude approval settings: %w", err)
		}
		permissionPromptTool = "mcp__" + approvalMCPServerName + "__" + driver.ApprovalToolName
		mcpServers := make(map[string]config.MCPServer, len(cfg.MCPServers)+1)
		maps.Copy(mcpServers, cfg.MCPServers)
		mcpServers[approvalMCPServerName] = config.MCPServer{
			Type: "http", URL: approvalBroker.URL(), Headers: approvalBroker.Headers(),
		}
		cfg.MCPServers = mcpServers
	}
	mcpJSON, err := cfg.MCPConfigJSON()
	if err != nil {
		if approvalBroker != nil {
			_ = approvalBroker.Close()
		}
		return nil, err
	}
	var mcpConfigPath string
	if len(mcpJSON) != 0 {
		if err := utils.EnsurePrivateDir(input.EphemeralDir); err != nil {
			if approvalBroker != nil {
				_ = approvalBroker.Close()
			}
			return nil, fmt.Errorf("prepare ephemeral MCP directory: %w", err)
		}
		mcpConfigPath = filepath.Join(input.EphemeralDir, "mcp.json")
		if err := utils.ReplacePrivateFile(mcpConfigPath, mcpJSON); err != nil {
			if approvalBroker != nil {
				_ = approvalBroker.Close()
			}
			return nil, fmt.Errorf("materialize Claude MCP configuration: %w", err)
		}
	}
	if input.SetupProfile != "" {
		if err := recordRuntimeSetup(input, mcpJSON, settingsPath); err != nil {
			if approvalBroker != nil {
				_ = approvalBroker.Close()
			}
			return nil, err
		}
	}
	return driver.NewProcessDriver(driver.ProcessConfig{
		Executable: cfg.ClaudeExecutable, ExpectedVersion: cfg.ExpectedClaudeVersion,
		StrictVersion: cfg.StrictVersion, Workspace: input.Workspace, Model: cfg.Model,
		AppendSystemPrompt: cfg.AppendSystemPrompt, AgentsJSON: agentsJSON, MCPConfigPath: mcpConfigPath,
		SettingsPath: settingsPath, PermissionPromptTool: permissionPromptTool, ApprovalBroker: approvalBroker,
		SkillRoot: skillRoot, PluginDirs: pluginDirs, Environment: environment,
		MaxEventBytes: cfg.MaxEventBytes, MaxStderrBytes: cfg.MaxStderrBytes,
		InterruptGrace: cfg.InterruptGrace(), AwaitTelemetry: awaitTelemetry,
	}), nil
}

func approvalServerNames(servers map[string]config.MCPServer) (protected []string) {
	for name, server := range servers {
		if server.RequireApproval {
			protected = append(protected, name)
		}
	}
	return protected
}

func materializeGoogleCredentials(environment []string, directory string) ([]string, error) {
	// The compiler injects the Secret value as JSON, while Google ADC expects a
	// file path. Keep the credential in ephemeral Actor storage rather than the
	// well-known path under /data, which is durable and may be snapshotted.
	prefix := config.GoogleCredentialsJSONEnvName + "="
	var credentials string
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if credentials != "" {
				return nil, fmt.Errorf("%s is configured more than once", config.GoogleCredentialsJSONEnvName)
			}
			credentials = strings.TrimPrefix(item, prefix)
			continue
		}
		filtered = append(filtered, item)
	}
	if credentials == "" {
		return filtered, nil
	}
	if !json.Valid([]byte(credentials)) {
		return nil, fmt.Errorf("%s must contain valid JSON", config.GoogleCredentialsJSONEnvName)
	}
	if err := utils.EnsurePrivateDir(directory); err != nil {
		return nil, fmt.Errorf("prepare ephemeral credentials directory: %w", err)
	}
	path := filepath.Join(directory, "google-credentials.json")
	if err := utils.ReplacePrivateFile(path, []byte(credentials)); err != nil {
		return nil, fmt.Errorf("materialize Google credentials: %w", err)
	}
	return setEnvironment(filtered, config.GoogleApplicationCredentialsEnvName, path), nil
}

// nativeTelemetryEnvironment turns on Claude Code telemetry for the signals the
// controller exports, and its content flags when capture is on. It reports
// whether telemetry is on.
func nativeTelemetryEnvironment(environment []string, telemetry tracing.RuntimeTelemetry) ([]string, bool) {
	exported := func(name string) bool {
		return slices.Contains(environment, name+"=otlp")
	}
	traces, logs := exported("OTEL_TRACES_EXPORTER"), exported("OTEL_LOGS_EXPORTER")
	if !traces && !logs && !exported("OTEL_METRICS_EXPORTER") {
		return environment, false
	}
	flags := []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"}
	if telemetry.CaptureContent {
		flags = append(flags, "OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS")
		if traces {
			flags = append(flags, "OTEL_LOG_TOOL_CONTENT")
		}
		if logs {
			flags = append(flags, "OTEL_LOG_ASSISTANT_RESPONSES")
		}
	}
	for _, flag := range flags {
		environment = setEnvironment(environment, flag, "1")
	}
	return environment, true
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

// SetupInstaller applies the fixed hook using the same native configuration path
// as normal execution. It never validates/starts a CLI or binds a continuation.
type SetupInstaller struct {
	Input Input
	Store *continuation.Store
	// Runner is the current driver, including its ephemeral approval broker.
	Runner *driver.ProcessDriver
	mu     sync.Mutex
	closed bool
}

func (s *SetupInstaller) Setup(ctx context.Context, input workspace.Preparation, observe bool) (workspace.Runner, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, "", fmt.Errorf("Claude setup installer is closed")
	}
	stateDir := filepath.Join(s.Input.DurableDir, ".kagent")
	if input.Provider != "claude" {
		return nil, "", fmt.Errorf("assigned native provider differs")
	}
	if observe {
		if err := workspace.CheckSetup(s.Input.ConfigJSON, stateDir, input, true); err != nil {
			return nil, "", err
		}
		configured := s.Input
		configured.SetupProfile = input.Profile
		if err := checkRuntimeSetup(configured, false); err != nil {
			return nil, "", err
		}
		return nil, "append_system_prompt", nil
	}
	if s.Store == nil {
		return nil, "", fmt.Errorf("continuation store is required")
	}
	if _, started, err := s.Store.Load(); err != nil || started {
		return nil, "", fmt.Errorf("native history already exists or is malformed")
	}
	configDigest, mcpDigest, err := workspace.ConfigDigests(s.Input.ConfigJSON)
	if err != nil || configDigest != input.ConfigDigest || mcpDigest != input.MCPDigest {
		return nil, "", fmt.Errorf("assigned compiler configuration differs")
	}
	configured := s.Input
	configured.SetupProfile = input.Profile
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
	return runner, "append_system_prompt", nil
}

// Close follows the configured driver when preparation replaces it, so the
// existing executor shutdown owns every approval broker it constructed.
func (s *SetupInstaller) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.Runner != nil {
		return s.Runner.Close()
	}
	return nil
}

// This private projection fingerprints materialized files, including the
// ephemeral authenticated approval bridge. It stores no broker token and is
// separate from the immutable original setup effect/receipt.
type runtimeSetup struct {
	Profile      string `json:"profile"`
	ConfigDigest string `json:"config_digest"`
	MCPDigest    string `json:"mcp_digest"`
	SetupDigest  string `json:"setup_digest"`
	MCPFile      string `json:"mcp_file"`
	SettingsFile string `json:"settings_file,omitempty"`
}

func runtimeSetupIdentity(input Input) (runtimeSetup, error) {
	configDigest, mcpDigest, err := workspace.ConfigDigests(input.ConfigJSON)
	if err != nil {
		return runtimeSetup{}, err
	}
	setupDigest, err := workspace.SetupDigest(input.SetupProfile)
	return runtimeSetup{Profile: input.SetupProfile, ConfigDigest: configDigest, MCPDigest: mcpDigest, SetupDigest: setupDigest}, err
}

func recordRuntimeSetup(input Input, mcpJSON []byte, settingsPath string) error {
	identity, err := runtimeSetupIdentity(input)
	if err != nil {
		return err
	}
	identity.MCPFile = workspace.Digest(mcpJSON)
	if settingsPath != "" {
		settings, err := os.ReadFile(settingsPath)
		if err != nil {
			return err
		}
		identity.SettingsFile = workspace.Digest(settings)
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	directory := filepath.Join(input.DurableDir, ".kagent")
	if err := utils.EnsurePrivateDir(directory); err != nil {
		return err
	}
	return utils.ReplacePrivateFile(filepath.Join(directory, "claude-runtime-setup.json"), data)
}

func checkRuntimeSetup(input Input, allowMissing bool) error {
	expected, err := runtimeSetupIdentity(input)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(input.DurableDir, ".kagent", "claude-runtime-setup.json"))
	if err != nil {
		return fmt.Errorf("read installed Claude runtime setup: %w", err)
	}
	var saved runtimeSetup
	if json.Unmarshal(data, &saved) != nil || saved.Profile != expected.Profile || saved.ConfigDigest != expected.ConfigDigest || saved.MCPDigest != expected.MCPDigest || saved.SetupDigest != expected.SetupDigest || len(saved.MCPFile) != 64 {
		return fmt.Errorf("installed Claude runtime setup differs")
	}
	cfg, err := config.Parse(input.ConfigJSON)
	if err != nil {
		return err
	}
	if (len(approvalServerNames(cfg.MCPServers)) != 0) != (len(saved.SettingsFile) == 64) {
		return fmt.Errorf("installed Claude approval settings differ")
	}
	for name, digest := range map[string]string{"mcp.json": saved.MCPFile, "settings.json": saved.SettingsFile} {
		if digest == "" {
			continue
		}
		actual, err := os.ReadFile(filepath.Join(input.EphemeralDir, name))
		if allowMissing && os.IsNotExist(err) {
			continue
		}
		if err != nil || workspace.Digest(actual) != digest {
			return fmt.Errorf("installed Claude %s differs", name)
		}
	}
	return nil
}
