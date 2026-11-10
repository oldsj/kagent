package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestNewMaterializesDurableDirectories(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	ephemeralDir := filepath.Join(t.TempDir(), "credentials")
	workspace := filepath.Join(durableDir, "workspace")
	runner, err := New(context.Background(), Input{
		ConfigJSON: []byte(`{"version":5,"claude_executable":"claude","expected_claude_version":"2.1.285","strict_version":true,"max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`),
		Workspace:  workspace,
		DurableDir: durableDir, EphemeralDir: ephemeralDir,
		Environment: []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/wrong", "DISABLE_UPDATES=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		t.Fatal("New() returned a nil runner")
	}
	for _, path := range []string{workspace, filepath.Join(durableDir, "claude")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s permissions = %o, want 700", path, info.Mode().Perm())
		}
	}
}

func TestNativeProcessRetriesAreOptIn(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "unset", want: "unset"},
		{name: "empty", environment: []string{env.TestClaudeDisableRetries + "="}, want: "unset"},
		{name: "false", environment: []string{env.TestClaudeDisableRetries + "=false"}, want: "unset"},
		{name: "true", environment: []string{env.TestClaudeDisableRetries + "=true"}, want: "0"},
		{name: "preserve native setting", environment: []string{config.MaxRetriesEnvName + "=10"}, want: "10"},
		{name: "override native setting", environment: []string{env.TestClaudeDisableRetries + "=true", config.MaxRetriesEnvName + "=10"}, want: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "claude")
			script := fmt.Sprintf("#!/bin/sh\n[ \"${CLAUDE_CODE_MAX_RETRIES-unset}\" = %s ] || exit 42\necho %s\n", test.want, config.PinnedClaudeVersion)
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Production("claude-test", "help")
			cfg.ClaudeExecutable = executable
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := New(t.Context(), Input{
				ConfigJSON: raw, Workspace: filepath.Join(dir, "workspace"),
				DurableDir: filepath.Join(dir, "data"), EphemeralDir: filepath.Join(dir, "ephemeral"), Environment: test.environment,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runner.Close() })
			if err := runner.Validate(t.Context()); err != nil {
				t.Fatalf("native process did not receive the expected retry setting: %v", err)
			}
		})
	}
}

func TestNewMaterializesSkillsAndMCPConfig(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	claudeDir := filepath.Join(durableDir, "claude")
	skillRoot := filepath.Join(durableDir, "generated", "claude")
	packageRoot := filepath.Join(claudeDir, "packages", "standalone-0")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "SKILL.md"), []byte("# Review"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Production("claude-test", "help")
	cfg.StrictVersion = false
	cfg.SkillResources = &agentplugin.Resources{Skills: []agentplugin.Skill{{
		Name: "review", Source: agentplugin.Source{Git: &agentplugin.GitSource{URL: "unused", Commit: strings.Repeat("a", 40)}},
	}}}
	cfg.MCPServers = map[string]config.MCPServer{"tools": {Type: "http", URL: "https://mcp.example.com/mcp"}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ephemeralDir := filepath.Join(t.TempDir(), "generated")
	runner, err := New(context.Background(), Input{
		ConfigJSON: raw, Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
		EphemeralDir: ephemeralDir, Environment: []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(skillRoot, ".claude", "skills", "review", "SKILL.md"): "# Review",
		filepath.Join(skillRoot, ".claude-plugin", "plugin.json"):           `{"name":"kagent-skills","skills":"./.claude/skills/"}`,
	} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v; want %q", path, contents, err, want)
		}
	}
	defer runner.Close()
	rawMCP, err := os.ReadFile(filepath.Join(ephemeralDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mcpConfig struct {
		Servers map[string]config.MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawMCP, &mcpConfig); err != nil {
		t.Fatal(err)
	}
	if len(mcpConfig.Servers) != 2 || mcpConfig.Servers["tools"].URL != "https://mcp.example.com/mcp" || mcpConfig.Servers[approvalMCPServerName].URL == "" {
		t.Fatalf("invalid MCP configuration")
	}
	if args := strings.Join(runner.Args(runtime.Turn{Prompt: "test"}), "\n"); !strings.Contains(args, "--plugin-dir\n"+skillRoot) {
		t.Fatalf("arguments do not expose materialized skills: %s", args)
	}
}

func TestNewMaterializesApprovalSettings(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	ephemeralDir := filepath.Join(t.TempDir(), "generated")
	cfg := config.Production("claude-test", "help")
	cfg.StrictVersion = false
	cfg.MCPServers = map[string]config.MCPServer{
		"protected": {Type: "http", URL: "https://mcp.example.com/write", RequireApproval: true},
		"readonly":  {Type: "http", URL: "https://mcp.example.com/read"},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(context.Background(), Input{
		ConfigJSON: raw, Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
		EphemeralDir: ephemeralDir, Environment: []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	settings, err := os.ReadFile(filepath.Join(ephemeralDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var approvalSettings struct {
		Permissions struct {
			Ask []string `json:"ask"`
		} `json:"permissions"`
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &approvalSettings); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(approvalSettings.Permissions.Ask, []string{"AskUserQuestion", "mcp__protected__*"}) || len(approvalSettings.Hooks.PreToolUse) != 1 || approvalSettings.Hooks.PreToolUse[0].Matcher != "*" {
		t.Fatalf("invalid durable approval settings: %s", settings)
	}

	mcpConfig, err := os.ReadFile(filepath.Join(ephemeralDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var materialized struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpConfig, &materialized); err != nil {
		t.Fatal(err)
	}
	approvalServer := materialized.Servers[approvalMCPServerName]
	if approvalServer.Type != "http" || !strings.HasPrefix(approvalServer.URL, "http://127.0.0.1:") ||
		!strings.HasPrefix(approvalServer.Headers["Authorization"], "Bearer ") {
		t.Fatalf("private approval MCP server = %#v", approvalServer)
	}
	if len(materialized.Servers) != 3 {
		t.Fatalf("materialized MCP servers = %#v", materialized.Servers)
	}
	args := strings.Join(runner.Args(runtime.Turn{Prompt: "test"}), "\n")
	for _, required := range []string{
		// Only the adapter-owned user scope loads; the checkout supplies
		// context and skills, never settings.
		"--setting-sources\nuser\n",
		"--add-dir\n" + runnerWorkspace(t, runner),
		"--plugin-dir\n" + filepath.Join(ephemeralDir, "workspace-skills"),
		"--disallowedTools\nScheduleWakeup,Monitor,CronCreate,CronList,CronDelete,RemoteTrigger",
		"--settings\n" + filepath.Join(ephemeralDir, "settings.json"),
		"--dangerously-skip-permissions",
		"--permission-prompt-tool\nmcp__" + approvalMCPServerName + "__approve",
	} {
		if !strings.Contains(args, required) {
			t.Fatalf("approval arguments do not contain %q: %s", required, args)
		}
	}
	for _, forbidden := range []string{"--bare", "--permission-mode", "dontAsk"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("approval arguments contain %q: %s", forbidden, args)
		}
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	input := Input{ConfigJSON: []byte(`{}`), Workspace: "relative", DurableDir: "relative", EphemeralDir: "relative"}
	if _, err := New(context.Background(), input); err == nil {
		t.Fatal("New() accepted invalid input")
	}
}

func TestMaterializeGoogleCredentials(t *testing.T) {
	dir := t.TempDir()
	raw := `{"type":"service_account","project_id":"test"}`
	environment, err := materializeGoogleCredentials([]string{"A=1", config.GoogleCredentialsJSONEnvName + "=" + raw}, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "google-credentials.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != raw {
		t.Fatalf("credentials = %q", contents)
	}
	if len(environment) != 2 || environment[0] != "A=1" || environment[1] != config.GoogleApplicationCredentialsEnvName+"="+path {
		t.Fatalf("environment = %v", environment)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v, %v", info, err)
	}
}

func TestSetEnvironmentOverridesExistingValue(t *testing.T) {
	got := setEnvironment([]string{"A=1", "A=2", "B=3"}, "A", "4")
	want := []string{"B=3", "A=4"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("setEnvironment() = %v, want %v", got, want)
	}
}

func TestNativeTelemetryEnvironmentFollowsExportedSignals(t *testing.T) {
	for name, test := range map[string]struct {
		environment []string
		capture     bool
		want        []string
		absent      []string
	}{
		"off": {
			environment: []string{"OTEL_TRACES_EXPORTER=none", "OTEL_METRICS_EXPORTER=none", "OTEL_LOGS_EXPORTER=none"},
			capture:     true,
			absent:      []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_LOG_USER_PROMPTS"},
		},
		"traces without capture": {
			environment: []string{"OTEL_TRACES_EXPORTER=otlp", "OTEL_LOGS_EXPORTER=none"},
			want:        []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"},
			absent:      []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_CONTENT"},
		},
		"traces with capture": {
			environment: []string{"OTEL_TRACES_EXPORTER=otlp", "OTEL_LOGS_EXPORTER=none"},
			capture:     true,
			want:        []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_TOOL_CONTENT"},
			absent:      []string{"OTEL_LOG_ASSISTANT_RESPONSES"},
		},
		"logs with capture": {
			environment: []string{"OTEL_TRACES_EXPORTER=none", "OTEL_LOGS_EXPORTER=otlp"},
			capture:     true,
			want:        []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_ASSISTANT_RESPONSES"},
			absent:      []string{"OTEL_LOG_TOOL_CONTENT"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, enabled := nativeTelemetryEnvironment(test.environment, tracing.RuntimeTelemetry{CaptureContent: test.capture})
			if enabled != slices.Contains(got, "CLAUDE_CODE_ENABLE_TELEMETRY=1") {
				t.Errorf("enabled = %t for %v", enabled, got)
			}
			for _, flag := range test.want {
				if !slices.Contains(got, flag+"=1") {
					t.Errorf("%s missing from %v", flag, got)
				}
			}
			for _, flag := range test.absent {
				if slices.Contains(got, flag+"=1") {
					t.Errorf("%s set in %v", flag, got)
				}
			}
		})
	}
}

func TestLargeSystemPromptIsDeliveredThroughFile(t *testing.T) {
	// Given instructions larger than Linux's per-argument limit, when the
	// adapter prepares a turn, assert that the CLI receives a private file
	// containing the complete prompt instead of the prompt text in argv.
	prompt := strings.Repeat("Helpful context. ", 8000) + "bootstrap-large-prompt-end"
	cfg := config.Config{Version: config.Version, ClaudeExecutable: "claude", AppendSystemPrompt: prompt, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGraceMillis: 100}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	runner, err := New(t.Context(), Input{ConfigJSON: raw, Workspace: filepath.Join(directory, "workspace"), DurableDir: filepath.Join(directory, "data"), EphemeralDir: filepath.Join(directory, "ephemeral")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	args := runner.Args(runtime.Turn{Prompt: "What is 2+2?"})
	index := slices.Index(args, "--append-system-prompt-file")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("missing prompt file argument: %v", args)
	}
	content, err := os.ReadFile(args[index+1])
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != prompt {
		t.Fatal("system prompt file lost compiled instructions")
	}
	info, err := os.Stat(args[index+1])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("prompt permissions = %o", info.Mode().Perm())
	}
	if slices.Contains(args, prompt) || slices.Contains(args, "--append-system-prompt") {
		t.Fatal("system prompt is still passed inline")
	}
}

// runnerWorkspace reads the workspace back from the driver's own arguments, so
// the assertion holds whatever directory the test configured.
func runnerWorkspace(t *testing.T, runner interface{ Args(runtime.Turn) []string }) string {
	t.Helper()
	args := runner.Args(runtime.Turn{Prompt: "test"})
	for i, arg := range args {
		if arg == "--add-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("arguments have no --add-dir: %v", args)
	return ""
}

func TestWorkspaceSkillsPluginLinksCheckoutSkills(t *testing.T) {
	ephemeral, workspace := t.TempDir(), t.TempDir()
	for range 2 { // A restart replaces the link rather than failing on it.
		root, err := materializeWorkspaceSkills(ephemeral, workspace)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
		if err != nil || string(manifest) != `{"name":"workspace"}` {
			t.Fatalf("plugin manifest = %q, %v; want a name and no hooks or MCP servers", manifest, err)
		}
		target, err := os.Readlink(filepath.Join(root, "skills"))
		if err != nil || target != filepath.Join(workspace, ".claude", "skills") {
			t.Fatalf("skills link = %q, %v", target, err)
		}
	}
}

// The headless policy reaches the native process whatever the Actor's own
// environment says: background and scheduled work stay off unless the
// configuration opts in, and the provider and workspace-memory switches are
// always the adapter's.
func TestHeadlessPolicyReachesClaudeProcess(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe defaults", true: "explicit opt in"}[allow], func(t *testing.T) {
			dir := t.TempDir()
			want := map[bool]string{false: "1 1 1 1", true: "0 0 1 1"}[allow]
			executable := filepath.Join(dir, "claude")
			script := fmt.Sprintf("#!/bin/sh\n[ \"$%s $%s $%s $%s\" = \"%s\" ] || exit 42\necho %s\n",
				config.DisableBackgroundTasksEnvName, config.DisableCronEnvName, config.ProviderManagedByHostEnvName, config.AdditionalDirectoriesMemoryEnvName,
				want, config.PinnedClaudeVersion)
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Production("", "")
			cfg.ClaudeExecutable = executable
			cfg.AllowBackgroundTasks, cfg.AllowScheduledTasks = allow, allow
			if allow {
				cfg.DisallowedTools = []string{"RemoteTrigger"}
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := New(t.Context(), Input{
				ConfigJSON: raw, Workspace: filepath.Join(dir, "workspace"), DurableDir: filepath.Join(dir, "data"), EphemeralDir: filepath.Join(dir, "generated"),
				Environment: []string{
					config.DisableBackgroundTasksEnvName + "=wrong", config.DisableCronEnvName + "=wrong",
					config.ProviderManagedByHostEnvName + "=0", config.AdditionalDirectoriesMemoryEnvName + "=0",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runner.Close() })
			if err := runner.Validate(t.Context()); err != nil {
				t.Fatalf("native process did not receive the headless policy: %v", err)
			}
			args := strings.Join(runner.Args(runtime.Turn{Prompt: "hello"}), "\n") + "\n"
			wantTools := map[bool]string{false: "ScheduleWakeup,Monitor,CronCreate,CronList,CronDelete,RemoteTrigger", true: "RemoteTrigger"}[allow]
			if !strings.Contains(args, "--disallowedTools\n"+wantTools+"\n") {
				t.Fatalf("arguments = %s, want disallowed tools %s", args, wantTools)
			}
		})
	}
}
