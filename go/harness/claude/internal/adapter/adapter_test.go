package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestNewMaterializesDurableDirectories(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	ephemeralDir := filepath.Join(t.TempDir(), "credentials")
	workspace := filepath.Join(durableDir, "workspace")
	runner, err := New(context.Background(), Input{
		ConfigJSON: []byte(`{"version":5,"claude_executable":"claude","expected_claude_version":"2.1.260","strict_version":true,"max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`),
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

func TestHeadlessPolicyReachesClaudeProcess(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe defaults", true: "explicit opt in"}[allow], func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "launch")
			executable := filepath.Join(dir, "claude")
			script := `#!/bin/sh
printf '%s\n' "$CLAUDE_CODE_DISABLE_BACKGROUND_TASKS" "$CLAUDE_CODE_DISABLE_CRON" "$CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST" "$@" > "$CAPTURE"
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success"}'
`
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Production("", "")
			cfg.ClaudeExecutable = executable
			cfg.AllowBackgroundTasks, cfg.AllowScheduledTasks = allow, allow
			if allow {
				cfg.DisallowedTools = []string{}
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := New(t.Context(), Input{
				ConfigJSON: raw, Workspace: filepath.Join(dir, "workspace"), DurableDir: filepath.Join(dir, "data"), EphemeralDir: filepath.Join(dir, "generated"),
				Environment: []string{"CAPTURE=" + capture, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=wrong", "CLAUDE_CODE_DISABLE_CRON=wrong", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=0"},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runner.Close() })
			// This fake emits only a result, which is returned as an outcome.
			outcome, err := runner.Run(t.Context(), runtime.Turn{Prompt: "hello"}, nil)
			if err != nil || outcome.Failure != nil || outcome.Pending != nil {
				t.Fatalf("Run() = %#v, %v", outcome, err)
			}
			launch, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if allow {
				if !strings.HasPrefix(string(launch), "0\n0\n1\n") || strings.Contains(string(launch), "--disallowedTools") {
					t.Fatalf("opt-in launch = %s", launch)
				}
			} else if !strings.HasPrefix(string(launch), "1\n1\n1\n") || !strings.Contains(string(launch), "--disallowedTools\nScheduleWakeup,Monitor,CronCreate,CronList,CronDelete,RemoteTrigger\n") {
				t.Fatalf("headless launch = %s", launch)
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
		filepath.Join(ephemeralDir, "mcp.json"):                             `{"mcpServers":{"tools":{"type":"http","url":"https://mcp.example.com/mcp"}}}`,
	} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v; want %q", path, contents, err, want)
		}
	}
	if args := strings.Join(runner.Args(runtime.Turn{Prompt: "test"}), "\n"); !strings.Contains(args, "--add-dir\n"+skillRoot) {
		t.Fatalf("arguments do not expose materialized skills: %s", args)
	}
}

func TestNewMaterializesApprovalSettings(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	ephemeralDir := filepath.Join(t.TempDir(), "generated")
	cfg := config.Production("claude-test", "help")
	cfg.StrictVersion = false
	cfg.MCPServers = map[string]config.MCPServer{
		"protected": {Type: "http", URL: "https://mcp.example.com/write", RequireApproval: true, TimeoutMillis: new(int64(600_000))},
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
	if got, want := string(settings), `{"disableAllHooks":true,"permissions":{"ask":["mcp__protected__*"]}}`; got != want {
		t.Fatalf("approval settings = %s, want %s", got, want)
	}
	mcpConfig, err := os.ReadFile(filepath.Join(ephemeralDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var materialized struct {
		Servers map[string]struct {
			Type          string            `json:"type"`
			URL           string            `json:"url"`
			Headers       map[string]string `json:"headers"`
			TimeoutMillis *int64            `json:"timeout"`
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
	if approvalServer.TimeoutMillis == nil || *approvalServer.TimeoutMillis != 100_000_000 {
		t.Fatalf("private approval timeout = %v, want 100000000 ms", approvalServer.TimeoutMillis)
	}
	protected := materialized.Servers["protected"]
	if protected.TimeoutMillis == nil || *protected.TimeoutMillis != 600_000 {
		t.Fatalf("protected server timeout = %v, want its configured 600000 ms", protected.TimeoutMillis)
	}
	if materialized.Servers["readonly"].TimeoutMillis != nil {
		t.Fatal("ordinary server without a timeout acquired an override")
	}
	if strings.Contains(string(mcpConfig), "require_approval") {
		t.Fatal("native MCP configuration contains internal approval policy")
	}
	args := strings.Join(runner.Args(runtime.Turn{Prompt: "test"}), "\n")
	for _, required := range []string{
		"--setting-sources\nuser,project\n",
		"--settings\n" + filepath.Join(ephemeralDir, "settings.json"),
		"--dangerously-skip-permissions",
		"--strict-mcp-config",
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
				if slices.ContainsFunc(got, func(variable string) bool { return strings.HasPrefix(variable, flag+"=") }) {
					t.Errorf("%s set in %v", flag, got)
				}
			}
		})
	}
}
