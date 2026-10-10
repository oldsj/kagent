package config

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestOwnsEnvironment(t *testing.T) {
	for _, name := range []string{
		AnthropicAPIKeyEnvName,
		ClaudeConfigDirEnvName,
		DisableBackgroundTasksEnvName,
		DisableCronEnvName,
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA",
		"OTEL_TRACES_EXPORTER",
		"OTEL_LOG_RAW_API_BODIES",
		"TRACEPARENT",
		MCPCredentialEnvPrefix + "ABC123",
	} {
		if !OwnsEnvironment(name) {
			t.Errorf("OwnsEnvironment(%q) = false", name)
		}
	}
	if OwnsEnvironment("USER_DEFINED") {
		t.Fatal("OwnsEnvironment accepted a user-defined name")
	}
	for _, name := range []string{"OTEL_TRACES_EXPORT_INTERVAL", "OTEL_RESOURCE_ATTRIBUTES"} {
		if OwnsEnvironment(name) {
			t.Errorf("OwnsEnvironment(%q) = true", name)
		}
	}
}

func TestProductionRoundTrip(t *testing.T) {
	cfg := Production("claude-test", "help")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ExpectedClaudeVersion != PinnedClaudeVersion || cfg.Model != "claude-test" || cfg.AppendSystemPrompt != "help" {
		t.Errorf("production config = %#v", cfg)
	}
}

func TestExecutionLimits(t *testing.T) {
	for _, test := range []struct {
		name, limits   string
		grace, ceiling time.Duration
		wantError      string
	}{
		{name: "defaults", grace: DefaultPostResultGrace, ceiling: DefaultTurnTimeout},
		{name: "custom", limits: `,"post_result_grace_millis":250,"turn_timeout_millis":9000`, grace: 250 * time.Millisecond, ceiling: 9 * time.Second},
		{name: "zero grace", limits: `,"post_result_grace_millis":0`, wantError: "post_result_grace_millis"},
		{name: "negative ceiling", limits: `,"turn_timeout_millis":-1`, wantError: "turn_timeout_millis"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Parse([]byte(`{"version":5,"claude_executable":"claude","max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100` + test.limits + `}`))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Parse error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PostResultGrace() != test.grace || cfg.TurnTimeout() != test.ceiling {
				t.Fatalf("execution limits = %#v", cfg)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(raw)
			if err != nil || !reflect.DeepEqual(parsed, cfg) {
				t.Fatalf("limits round trip = %#v, %v", parsed, err)
			}
		})
	}
	cfg := Production("", "")
	if cfg.PostResultGrace() != DefaultPostResultGrace || cfg.TurnTimeout() != DefaultTurnTimeout {
		t.Fatalf("production execution limits = %#v", cfg)
	}
	if strconv.IntSize == 64 {
		cfg.PostResultGraceMillis = int(int64(math.MaxInt64)/int64(time.Millisecond)) + 1
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate accepted an overflowing duration")
		}
	}
}

func TestHeadlessPolicyDefaultsAndOverrides(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy string
		allow  bool
		tools  []string
	}{
		{name: "default", tools: []string{"ScheduleWakeup", "Monitor", "CronCreate", "CronList", "CronDelete", "RemoteTrigger"}},
		{name: "explicit opt in", policy: `,"allow_background_tasks":true,"allow_scheduled_tasks":true,"disallowed_tools":[]`, allow: true, tools: []string{}},
		{name: "custom deny list", policy: `,"disallowed_tools":["Monitor","RemoteTrigger"]`, tools: []string{"Monitor", "RemoteTrigger"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Parse([]byte(`{"version":5,"claude_executable":"claude","max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100` + test.policy + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AllowBackgroundTasks != test.allow || cfg.AllowScheduledTasks != test.allow || !reflect.DeepEqual(cfg.DisallowedTools, test.tools) {
				t.Fatalf("headless policy = %#v", cfg)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(raw)
			if err != nil || !reflect.DeepEqual(parsed, cfg) {
				t.Fatalf("policy round trip = %#v, %v, want %#v", parsed, err, cfg)
			}
		})
	}
	production := Production("", "")
	if production.AllowBackgroundTasks || production.AllowScheduledTasks || !reflect.DeepEqual(production.DisallowedTools, defaultDisallowedTools()) {
		t.Fatalf("production headless policy = %#v", production)
	}
	for _, tool := range []string{"", " Monitor", "Monitor,CronCreate", "Bash(sleep *)"} {
		cfg := Production("", "")
		cfg.DisallowedTools = []string{tool}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate() accepted disallowed tool %q", tool)
		}
	}
}

func TestMCPConfigJSONAndSkillsRoundTrip(t *testing.T) {
	cfg := Production("claude-test", "help")
	cfg.SkillResources = &agentplugin.Resources{Skills: []agentplugin.Skill{{
		Name: "review", Source: agentplugin.Source{Git: &agentplugin.GitSource{URL: "https://example.com/repo", Commit: strings.Repeat("a", 40)}},
	}}}
	cfg.MCPServers = map[string]MCPServer{"tools": {
		Type: "http", URL: "https://mcp.example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}, RequireApproval: true,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := cfg.MCPConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"mcpServers":{"tools":{"type":"http","url":"https://mcp.example.com/mcp","headers":{"Authorization":"Bearer ${TOKEN}"}}}}`
	if string(raw) != want {
		t.Fatalf("MCPConfigJSON() = %s, want %s", raw, want)
	}
}

func TestAgentsJSON(t *testing.T) {
	cfg := Production("claude-test", "help")
	cfg.Agents = map[string]Agent{
		"reviewer": {
			Description: "Reviews changes", Prompt: "Review carefully", Model: "claude-child",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := cfg.AgentsJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"reviewer":{"description":"Reviews changes","prompt":"Review carefully","model":"claude-child"}}`
	if raw != want {
		t.Fatalf("AgentsJSON() = %s, want %s", raw, want)
	}
	parsed, err := Parse([]byte(`{"version":5,"claude_executable":"claude","expected_claude_version":"2.1.260","strict_version":true,"agents":` + raw + `,"max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Agents, cfg.Agents) {
		t.Fatalf("parsed agents = %#v, want %#v", parsed.Agents, cfg.Agents)
	}
}

func TestMCPTimeoutRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name      string
		timeout   *int64
		wantError bool
	}{
		{name: "absent"},
		{name: "minimum", timeout: new(int64(1000))},
		{name: "human wait", timeout: new(int64(100_000_000))},
		{name: "maximum timer delay", timeout: new(int64(2_147_483_647))},
		{name: "zero is not disabled", timeout: new(int64(0)), wantError: true},
		{name: "negative", timeout: new(int64(-1)), wantError: true},
		{name: "below native minimum", timeout: new(int64(999)), wantError: true},
		{name: "timer overflow", timeout: new(int64(2_147_483_648)), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Production("claude-test", "help")
			cfg.MCPServers = map[string]MCPServer{"tools": {
				Type: "http", URL: "https://mcp.example.com/mcp", RequireApproval: true, TimeoutMillis: test.timeout,
			}}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(raw)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "timeout must be between 1000") {
					t.Fatalf("Parse() error = %v, want invalid timeout", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			native, err := parsed.MCPConfigJSON()
			if err != nil {
				t.Fatal(err)
			}
			var projection struct {
				Servers map[string]json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal(native, &projection); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(projection.Servers["tools"], &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields["require_approval"]; ok {
				t.Fatal("native JSON contains internal approval policy")
			}
			value, present := fields["timeout"]
			if test.timeout == nil {
				if present {
					t.Fatalf("absent timeout serialized as %s", value)
				}
			} else {
				var got int64
				if !present || json.Unmarshal(value, &got) != nil || got != *test.timeout {
					t.Fatalf("native timeout = %s, want %d", value, *test.timeout)
				}
			}
		})
	}
}

func TestConfigRejectsInvalidAgents(t *testing.T) {
	tests := []map[string]Agent{
		{"": {Description: "description", Prompt: "prompt"}},
		{"not valid": {Description: "description", Prompt: "prompt"}},
		{"reviewer": {Prompt: "prompt"}},
	}
	for _, agents := range tests {
		cfg := Production("claude-test", "help")
		cfg.Agents = agents
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate() accepted agents %#v", agents)
		}
	}
}

func TestParseValidates(t *testing.T) {
	contents := `{"version":5,"claude_executable":"claude","expected_claude_version":"2.1.260","strict_version":true,"model":"claude-test","append_system_prompt":"help","max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`
	cfg, err := Parse([]byte(contents))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "claude-test" || cfg.AppendSystemPrompt != "help" {
		t.Errorf("parsed config = %#v", cfg)
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(`{"version":5,"surprise":true}`)); err == nil {
		t.Fatal("Parse() accepted an unknown field")
	}
}

func TestConfigRejectsTrailingValue(t *testing.T) {
	if _, err := Parse([]byte(`{} {}`)); err == nil {
		t.Fatal("Parse() accepted a trailing JSON value")
	}
}

func TestConfigRejectsMissingLimits(t *testing.T) {
	_, err := Parse([]byte(`{"version":5,"claude_executable":"claude"}`))
	if err == nil || !strings.Contains(err.Error(), "limits must be positive") {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestConfigValidatesRuntimeTelemetry(t *testing.T) {
	base := Production("claude-sonnet-4-5", "help")
	for _, test := range []struct {
		name      string
		telemetry tracing.RuntimeTelemetry
		wantError bool
	}{
		{name: "absent"},
		{name: "claude identity", telemetry: tracing.RuntimeTelemetry{
			Runtime: tracing.RuntimeClaude, AgentName: "assistant-claude", AgentNamespace: "kagent",
		}},
		{name: "another runtime", telemetry: tracing.RuntimeTelemetry{Runtime: tracing.RuntimeCodex}, wantError: true},
		{name: "capture above the ceiling", telemetry: tracing.RuntimeTelemetry{
			Runtime: tracing.RuntimeClaude, CaptureContent: true, MaxCaptureBytes: tracing.MaxCaptureBytes + 1,
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := base
			config.RuntimeTelemetry = test.telemetry
			if err := config.Validate(); (err != nil) != test.wantError {
				t.Fatalf("Validate() error = %v, wantError = %v", err, test.wantError)
			}
		})
	}
}

// A configuration without the telemetry section stays readable, which keeps
// standalone harness validation working.
func TestParseAcceptsConfigWithoutRuntimeTelemetry(t *testing.T) {
	config, err := Parse([]byte(`{"version":5,"claude_executable":"claude","expected_claude_version":"2.1.260","strict_version":true,"max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.RuntimeTelemetry != (tracing.RuntimeTelemetry{}) {
		t.Fatalf("runtime telemetry = %#v, want the zero value", config.RuntimeTelemetry)
	}
	if config.RuntimeTelemetry.CaptureLimit() != 0 {
		t.Fatal("capture is enabled without a telemetry section")
	}
}
