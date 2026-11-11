// Package config defines the versioned, non-secret Claude Harness runtime
// configuration shared by its compiler and Actor entrypoint.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// EffortLevels are the reasoning-effort values the pinned Claude Code CLI's
// --effort flag accepts.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

const (
	Version = 5
	// PinnedClaudeVersion is the Claude Code release the image installs. A bump
	// must recheck the telemetry gate in the driver.
	PinnedClaudeVersion                 = "2.1.285"
	ClaudeConfigDirEnvName              = "CLAUDE_CONFIG_DIR"
	DisableUpdatesEnvName               = "DISABLE_UPDATES"
	MaxRetriesEnvName                   = "CLAUDE_CODE_MAX_RETRIES"
	GoogleApplicationCredentialsEnvName = "GOOGLE_APPLICATION_CREDENTIALS"
	GoogleCredentialsJSONEnvName        = "KAGENT_CLAUDE_GOOGLE_CREDENTIALS_JSON"
	UseBedrockEnvName                   = "CLAUDE_CODE_USE_BEDROCK"
	UseVertexEnvName                    = "CLAUDE_CODE_USE_VERTEX"
	AWSRegionEnvName                    = "AWS_REGION"
	AWSAccessKeyEnvName                 = "AWS_ACCESS_KEY_ID"
	AWSSecretKeyEnvName                 = "AWS_SECRET_ACCESS_KEY"
	AWSSessionTokenEnvName              = "AWS_SESSION_TOKEN"
	AWSBedrockTokenEnvName              = "AWS_BEARER_TOKEN_BEDROCK"
	AnthropicAPIKeyEnvName              = "ANTHROPIC_API_KEY"
	ClaudeCodeOAuthTokenEnvName         = "CLAUDE_CODE_OAUTH_TOKEN"
	AnthropicBaseURLEnvName             = "ANTHROPIC_BASE_URL"
	VertexProjectEnvName                = "ANTHROPIC_VERTEX_PROJECT_ID"
	VertexRegionEnvName                 = "CLOUD_ML_REGION"
	SandboxEnvName                      = "IS_SANDBOX"
	DisableBackgroundTasksEnvName       = "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"
	DisableCronEnvName                  = "CLAUDE_CODE_DISABLE_CRON"
	ProviderManagedByHostEnvName        = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"
	AdditionalDirectoriesMemoryEnvName  = "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"
	MCPCredentialEnvPrefix              = "KAGENT_CLAUDE_MCP_CREDENTIAL_"
	// DefaultPostResultGrace bounds native activity after a turn's first result.
	DefaultPostResultGrace = 120 * time.Second
	// DefaultTurnTimeout bounds one native Claude process.
	DefaultTurnTimeout = 2 * time.Hour
)

// OwnsEnvironment reports whether the compiler or adapter reserves name for
// Claude runtime configuration. Harness authors cannot override these values.
func OwnsEnvironment(name string) bool {
	if strings.HasPrefix(name, MCPCredentialEnvPrefix) {
		return true
	}
	switch name {
	case ClaudeConfigDirEnvName, DisableUpdatesEnvName, MaxRetriesEnvName, GoogleApplicationCredentialsEnvName,
		GoogleCredentialsJSONEnvName, UseBedrockEnvName, UseVertexEnvName, AWSRegionEnvName,
		AWSAccessKeyEnvName, AWSSecretKeyEnvName, AWSSessionTokenEnvName, AWSBedrockTokenEnvName,
		AnthropicAPIKeyEnvName, ClaudeCodeOAuthTokenEnvName, AnthropicBaseURLEnvName, VertexProjectEnvName, VertexRegionEnvName,
		SandboxEnvName, DisableBackgroundTasksEnvName, DisableCronEnvName, ProviderManagedByHostEnvName, AdditionalDirectoriesMemoryEnvName, "CLAUDE_CODE_ENABLE_TELEMETRY",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", env.OTELTraces.Exporter.Name(), env.OTELMetrics.Exporter.Name(),
		env.OTELLogs.Exporter.Name(), env.OTELLogUserPrompts.Name(), env.OTELLogToolDetails.Name(), env.OTELLogToolContent.Name(),
		env.OTELLogAssistantResponses.Name(), env.OTELLogRawAPIBodies.Name(), "TRACEPARENT", "TRACESTATE",
		env.OTELExporterPrometheusHost.Name(), env.OTELExporterPrometheusPort.Name():
		return true
	default:
		return false
	}
}

type Config struct {
	MaxContentLength      *int64 `json:"max_content_length,omitempty"`
	ControlPlaneURL       string `json:"control_plane_url"`
	LogLevel              string `json:"log_level,omitempty"`
	Version               int    `json:"version"`
	ClaudeExecutable      string `json:"claude_executable"`
	ExpectedClaudeVersion string `json:"expected_claude_version"`
	StrictVersion         bool   `json:"strict_version"`
	// SDKRunner locates the Agent SDK bridge for KAGENT_CLAUDE_DRIVER=sdk.
	// Absent selects the harness image's Node on PATH and its bridge under
	// /opt/kagent/sdk-runner; a composed runtime points both into the payload.
	SDKRunner          *SDKRunner             `json:"sdk_runner,omitempty"`
	Model              string                 `json:"model,omitempty"`
	AppendSystemPrompt string                 `json:"append_system_prompt,omitempty"`
	Agents             map[string]Agent       `json:"agents,omitempty"`
	SkillResources     *agentplugin.Resources `json:"skill_resources,omitempty"`
	MCPServers         map[string]MCPServer   `json:"mcp_servers,omitempty"`
	// Git is the repository policy for Session workspaces. Absent means Sessions
	// cannot request a workspace.
	Git *workspace.Git `json:"git,omitempty"`
	// RuntimeTelemetry carries the compiler-owned span identity and content
	// capture policy. It is absent for standalone runs, which fall back to the
	// environment for service identity and leave capture disabled.
	RuntimeTelemetry     tracing.RuntimeTelemetry `json:"runtime_telemetry,omitzero"`
	MaxEventBytes        int                      `json:"max_event_bytes"`
	MaxStderrBytes       int                      `json:"max_stderr_bytes"`
	InterruptGraceMillis int                      `json:"interrupt_grace_millis"`
	// PostResultGraceMillis and TurnTimeoutMillis override the driver's
	// execution bounds. Zero selects DefaultPostResultGrace and DefaultTurnTimeout.
	PostResultGraceMillis int `json:"post_result_grace_millis,omitempty"`
	TurnTimeoutMillis     int `json:"turn_timeout_millis,omitempty"`
	// AllowBackgroundTasks and AllowScheduledTasks opt a standalone
	// configuration in to work that outlives a tool call. The compiler leaves
	// both off; enabling one also needs its tools removed from DisallowedTools.
	AllowBackgroundTasks bool `json:"allow_background_tasks,omitempty"`
	AllowScheduledTasks  bool `json:"allow_scheduled_tasks,omitempty"`
	// DisallowedTools replaces the default set of built-in tools Claude may
	// not use. Empty selects DefaultDisallowedTools.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
}

// SDKRunner is the Node executable and bridge script of the Agent SDK driver.
type SDKRunner struct {
	Node   string `json:"node"`
	Script string `json:"script"`
	// ICUDataDir holds the ICU data a system-ICU Node build loads at run time.
	// Empty keeps Node's compiled-in location.
	ICUDataDir string `json:"icu_data_dir,omitempty"`
}

// DefaultDisallowedTools are built-in tools that schedule work beyond the
// turn that called them. A headless turn ends with its process, so that work
// would be lost without the caller knowing.
func DefaultDisallowedTools() []string {
	return []string{"ScheduleWakeup", "Monitor", "CronCreate", "CronList", "CronDelete", "RemoteTrigger"}
}

// MCPServer is one compiler-owned direct remote server. Claude's strict MCP
// file is generated by the adapter; no project or user MCP configuration is
// inherited.
type MCPServer struct {
	Type            string            `json:"type"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers,omitempty"`
	RequireApproval bool              `json:"require_approval,omitempty"`
}

// Agent is one compiler-owned local Claude subagent passed through --agents.
// Permission mode, hooks, memory, isolation, and inline MCP are deliberately
// omitted from the supported contract.
type Agent struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model,omitempty"`
}

// Bounds the memory one stream-json line can take. Tool results carrying
// images run to a few megabytes.
const maxEventBytes = 16 << 20

func Production(model, instruction string) Config {
	return Config{
		Version: Version, ClaudeExecutable: "claude",
		ExpectedClaudeVersion: PinnedClaudeVersion, StrictVersion: true,
		Model: model, AppendSystemPrompt: instruction,
		MaxEventBytes: maxEventBytes, MaxStderrBytes: 64 << 10,
		InterruptGraceMillis: 2000,
	}
}

func Parse(b []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Config{}, fmt.Errorf("decode config: trailing JSON value")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.MaxContentLength != nil && *c.MaxContentLength < 0 {
		return fmt.Errorf("max_content_length must be nonnegative")
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn, or error")
	}
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, Version)
	}
	if strings.TrimSpace(c.ClaudeExecutable) == "" {
		return fmt.Errorf("claude_executable is required")
	}
	if c.StrictVersion && strings.TrimSpace(c.ExpectedClaudeVersion) == "" {
		return fmt.Errorf("expected_claude_version is required when strict_version is enabled")
	}
	if c.SDKRunner != nil && (!filepath.IsAbs(c.SDKRunner.Node) || !filepath.IsAbs(c.SDKRunner.Script) ||
		(c.SDKRunner.ICUDataDir != "" && !filepath.IsAbs(c.SDKRunner.ICUDataDir))) {
		return fmt.Errorf("sdk_runner node, script and icu_data_dir must be absolute paths")
	}
	if c.MaxEventBytes <= 0 || c.MaxStderrBytes <= 0 || c.InterruptGraceMillis <= 0 {
		return fmt.Errorf("event, stderr, and interrupt grace limits must be positive")
	}
	for _, limit := range []struct {
		name   string
		millis int
	}{
		{"post_result_grace_millis", c.PostResultGraceMillis},
		{"turn_timeout_millis", c.TurnTimeoutMillis},
	} {
		if limit.millis < 0 || int64(limit.millis) > math.MaxInt64/int64(time.Millisecond) {
			return fmt.Errorf("%s must be a duration in milliseconds", limit.name)
		}
	}
	for _, tool := range c.DisallowedTools {
		if !agentNamePattern.MatchString(tool) {
			return fmt.Errorf("disallowed tool %q must be a bare tool name", tool)
		}
	}
	if err := c.RuntimeTelemetry.Validate(); err != nil {
		return err
	}
	if c.Git != nil {
		if err := c.Git.Validate(); err != nil {
			return err
		}
	}
	if runtime := c.RuntimeTelemetry.Runtime; runtime != "" && runtime != tracing.RuntimeClaude {
		return fmt.Errorf("claude runtime telemetry names runtime %q", runtime)
	}
	for name, agent := range c.Agents {
		if !agentNamePattern.MatchString(name) {
			return fmt.Errorf("claude agent name %q must contain only letters, numbers, underscores, or hyphens", name)
		}
		if strings.TrimSpace(agent.Description) == "" || strings.TrimSpace(agent.Prompt) == "" {
			return fmt.Errorf("claude agent %q requires a non-empty description and prompt", name)
		}
	}
	for name, server := range c.MCPServers {
		if !agentNamePattern.MatchString(name) {
			return fmt.Errorf("claude MCP server name %q must contain only letters, numbers, underscores, or hyphens", name)
		}
		if server.Type != "http" && server.Type != "sse" {
			return fmt.Errorf("claude MCP server %q has unsupported transport %q", name, server.Type)
		}
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("claude MCP server %q URL is required", name)
		}
		for header, value := range server.Headers {
			if strings.TrimSpace(header) == "" || strings.TrimSpace(value) == "" {
				return fmt.Errorf("claude MCP server %q headers require non-empty names and values", name)
			}
		}
	}
	return nil
}

func (c Config) AgentsJSON() (string, error) {
	if len(c.Agents) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(c.Agents)
	if err != nil {
		return "", fmt.Errorf("encode Claude agents: %w", err)
	}
	return string(raw), nil
}

func (c Config) MCPConfigJSON() ([]byte, error) {
	if len(c.MCPServers) == 0 {
		return nil, nil
	}
	type nativeMCPServer struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	servers := make(map[string]nativeMCPServer, len(c.MCPServers))
	for name, server := range c.MCPServers {
		servers[name] = nativeMCPServer{Type: server.Type, URL: server.URL, Headers: server.Headers}
	}
	raw, err := json.Marshal(struct {
		Servers map[string]nativeMCPServer `json:"mcpServers"`
	}{Servers: servers})
	if err != nil {
		return nil, fmt.Errorf("encode Claude MCP configuration: %w", err)
	}
	return raw, nil
}

func (c Config) InterruptGrace() time.Duration {
	return time.Duration(c.InterruptGraceMillis) * time.Millisecond
}

// PostResultGrace bounds native activity after a turn's first result.
func (c Config) PostResultGrace() time.Duration {
	if c.PostResultGraceMillis == 0 {
		return DefaultPostResultGrace
	}
	return time.Duration(c.PostResultGraceMillis) * time.Millisecond
}

// TurnTimeout bounds one native Claude process.
func (c Config) TurnTimeout() time.Duration {
	if c.TurnTimeoutMillis == 0 {
		return DefaultTurnTimeout
	}
	return time.Duration(c.TurnTimeoutMillis) * time.Millisecond
}

// DisallowedBuiltinTools is the effective list of built-in tools Claude may not use.
func (c Config) DisallowedBuiltinTools() []string {
	if len(c.DisallowedTools) == 0 {
		return DefaultDisallowedTools()
	}
	return c.DisallowedTools
}
