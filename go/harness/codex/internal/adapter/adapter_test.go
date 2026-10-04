package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/pelletier/go-toml/v2"
)

func TestChatGPTMaterializesOnlySyntheticAuth(t *testing.T) {
	durable := t.TempDir()
	cfg := config.Production("gpt-6.1", "Reply briefly")
	cfg.Provider = config.Provider{Name: "chatgpt", AccountID: "test-account"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(t.Context(), Input{ConfigJSON: raw, Workspace: filepath.Join(durable, "workspace"), DurableDir: durable})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(durable, "codex", "auth.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var auth placeholderAuthFile
	if err := json.Unmarshal(contents, &auth); err != nil {
		t.Fatal(err)
	}
	if auth.AuthMode != "chatgpt" || auth.Tokens.AccountID != "test-account" || auth.Tokens.RefreshToken != "" || time.Since(auth.LastRefresh) > time.Minute {
		t.Fatal("incorrect synthetic authentication metadata")
	}
	for _, token := range []string{auth.Tokens.IDToken, auth.Tokens.AccessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatal("placeholder is not a JWT")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Expires int64 `json:"exp"`
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.Expires < time.Now().Add(24*time.Hour).Unix() {
			t.Fatal("placeholder expiry is not in the future")
		}
		if parts[2] != base64.RawURLEncoding.EncodeToString([]byte("inert-kagent-placeholder")) {
			t.Fatal("unexpected placeholder signature")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("auth file is not private")
	}
	contents, err = os.ReadFile(filepath.Join(durable, "codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var native nativeConfig
	if err := toml.Unmarshal(contents, &native); err != nil {
		t.Fatal(err)
	}
	if native.ModelProvider != "openai" || len(native.ModelProviders) != 0 || bytes.Contains(contents, []byte("OPENAI_API_KEY")) {
		t.Fatal("ChatGPT does not use the built-in provider")
	}
}

func TestChatGPTHTTPSKeepsSubscriptionAuth(t *testing.T) {
	cfg := config.Production("gpt-6.1-sol", "Reply briefly")
	cfg.Provider = config.Provider{Name: "chatgpt", AccountID: "test-account", ResponsesTransport: "https"}
	contents, err := renderConfig(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var native nativeConfig
	if err := toml.Unmarshal(contents, &native); err != nil {
		t.Fatal(err)
	}
	provider := native.ModelProviders[native.ModelProvider]
	if native.ModelProvider != "kagent-chatgpt" || !provider.RequiresOpenAIAuth || provider.EnvKey != "" || provider.BaseURL != "https://chatgpt.com/backend-api/codex" || provider.SupportsWebSockets == nil || *provider.SupportsWebSockets {
		t.Fatalf("HTTPS lost subscription authentication or enabled WebSockets: %#v", provider)
	}
}

func TestChatGPTBaseURLOverridesBackend(t *testing.T) {
	for _, test := range []struct {
		transport  string
		websockets bool
	}{{"", true}, {"https", false}} {
		cfg := config.Production("gpt-6.1", "Reply briefly")
		cfg.Provider = config.Provider{Name: "chatgpt", AccountID: "test-account", BaseURL: "https://codex.example.com/backend-api/codex", ResponsesTransport: test.transport}
		contents, err := renderConfig(cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var native nativeConfig
		if err := toml.Unmarshal(contents, &native); err != nil {
			t.Fatal(err)
		}
		provider := native.ModelProviders[native.ModelProvider]
		if native.ModelProvider != "kagent-chatgpt" || !provider.RequiresOpenAIAuth || provider.EnvKey != "" || provider.BaseURL != cfg.Provider.BaseURL || provider.SupportsWebSockets == nil || *provider.SupportsWebSockets != test.websockets {
			t.Fatalf("transport %q: incorrect ChatGPT provider: %#v", test.transport, provider)
		}
	}
}

func TestNewMaterializesCompilerOwnedConfiguration(t *testing.T) {
	durable := filepath.Join(t.TempDir(), "data")
	cfg := config.Production("gpt-5.2-codex", "line one\nline two")
	cfg.Provider = config.Provider{Name: "openai", BaseURL: "https://gateway.example.com/v1"}
	cfg.Agents = map[string]config.Agent{"reviewer": {Description: "Review carefully", Instruction: "Inspect \"all\" changes", Model: "gpt-5.2-codex"}}
	cfg.MCPServers = map[string]config.MCPServer{"tools": {
		URL: "https://mcp.example.com/mcp", Headers: map[string]string{"X-Tenant": "test", "Authorization": "${KAGENT_CODEX_MCP_CREDENTIAL_ABC}"}, EnabledTools: []string{"read"}, RequireApproval: true,
	}}
	cfg.Telemetry = &config.Telemetry{
		CaptureContent: true,
		Traces:         &config.OTLPExporter{Endpoint: "http://collector:4318/v1/traces", Protocol: "http/protobuf"},
		Logs:           &config.OTLPExporter{Endpoint: "http://logs:4317", Protocol: "grpc"},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(context.Background(), Input{ConfigJSON: raw, Workspace: filepath.Join(durable, "workspace"), DurableDir: durable, Environment: []string{"PATH=/bin", "CODEX_HOME=/wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		t.Fatal("New() returned nil")
	}
	codexHome := filepath.Join(durable, "codex")
	configPath := filepath.Join(codexHome, "config.toml")
	agentPath := filepath.Join(codexHome, "agents", "reviewer.toml")
	for _, path := range []string{configPath, agentPath} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %v, %v", path, info, err)
		}
	}
	configContents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configContents, []byte("environment =")) {
		t.Fatalf("generated OTEL configuration overrides Codex's environment default:\n%s", configContents)
	}
	var native nativeConfig
	if err := toml.Unmarshal(configContents, &native); err != nil {
		t.Fatalf("decode generated Codex configuration: %v", err)
	}
	provider := native.ModelProviders["kagent-openai"]
	server := native.MCPServers["tools"]
	agent := native.Agents["reviewer"]
	if native.ModelProvider != "kagent-openai" || provider.WireAPI != "responses" || provider.EnvKey != "OPENAI_API_KEY" || provider.BaseURL != cfg.Provider.BaseURL {
		t.Fatalf("generated provider configuration = %#v, provider name = %q", provider, native.ModelProvider)
	}
	if !native.Features.DefaultModeRequestUserInput {
		t.Fatal("generated Codex configuration does not enable request_user_input in default mode")
	}
	if native.Otel == nil || !native.Otel.LogUserPrompt ||
		native.Otel.TraceExporter == nil || native.Otel.TraceExporter.OTLPHTTP == nil ||
		native.Otel.TraceExporter.OTLPHTTP.Endpoint != cfg.Telemetry.Traces.Endpoint || native.Otel.TraceExporter.OTLPHTTP.Protocol != "binary" ||
		native.Otel.Exporter == nil || native.Otel.Exporter.OTLPGRPC == nil || native.Otel.Exporter.OTLPGRPC.Endpoint != cfg.Telemetry.Logs.Endpoint {
		t.Fatalf("generated OTEL configuration = %#v", native.Otel)
	}
	if native.ApprovalPolicy.Granular != (nativeGranularApprovalPolicy{MCPElicitations: true}) {
		t.Fatalf("generated approval policy = %#v", native.ApprovalPolicy)
	}
	if server.DefaultToolsApprovalMode != "prompt" || server.EnvHTTPHeaders["Authorization"] != "KAGENT_CODEX_MCP_CREDENTIAL_ABC" || server.HTTPHeaders["X-Tenant"] != "test" || len(server.EnabledTools) != 1 || server.EnabledTools[0] != "read" {
		t.Fatalf("generated MCP server configuration = %#v", server)
	}
	if agent.Description != "Review carefully" || agent.ConfigFile != agentPath {
		t.Fatalf("generated agent registration = %#v", agent)
	}
	agentContents, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	var agentConfig nativeAgentConfig
	if err := toml.Unmarshal(agentContents, &agentConfig); err != nil {
		t.Fatalf("decode generated Codex agent configuration: %v", err)
	}
	if agentConfig.Model != "gpt-5.2-codex" || agentConfig.DeveloperInstructions != `Inspect "all" changes` {
		t.Fatalf("generated Codex agent configuration = %#v", agentConfig)
	}
}

func TestRenderConfigOmitsDisabledSignalExporter(t *testing.T) {
	tests := []struct {
		name       string
		telemetry  *config.Telemetry
		wantLogs   bool
		wantTraces bool
	}{
		{
			name: "logs only",
			telemetry: &config.Telemetry{
				Logs: &config.OTLPExporter{Endpoint: "http://logs:4317", Protocol: "grpc"},
			},
			wantLogs: true,
		},
		{
			name: "traces only",
			telemetry: &config.Telemetry{
				Traces: &config.OTLPExporter{Endpoint: "http://traces:4318/v1/traces", Protocol: "http/protobuf"},
			},
			wantTraces: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Production("gpt-5.2-codex", "work carefully")
			cfg.Provider = config.Provider{Name: "openai"}
			cfg.Telemetry = test.telemetry
			contents, err := renderConfig(cfg, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var native nativeConfig
			if err := toml.Unmarshal(contents, &native); err != nil {
				t.Fatal(err)
			}
			if native.Otel == nil || (native.Otel.Exporter != nil) != test.wantLogs || (native.Otel.TraceExporter != nil) != test.wantTraces {
				t.Fatalf("generated OTEL configuration = %#v", native.Otel)
			}
		})
	}
}

func TestNewRejectsSymlinkedCodexHome(t *testing.T) {
	durable := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(durable, "codex")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Production("model", "instruction")
	cfg.Provider = config.Provider{Name: "openai"}
	raw, _ := json.Marshal(cfg)
	if _, err := New(context.Background(), Input{ConfigJSON: raw, Workspace: filepath.Join(durable, "workspace"), DurableDir: durable}); err == nil {
		t.Fatal("New() accepted symlinked Codex home")
	}
}

func TestPinnedCodexAcceptsGeneratedConfiguration(t *testing.T) {
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("pinned Codex CLI is not installed")
	}
	tests := []struct {
		name     string
		provider config.Provider
	}{
		{name: "OpenAI", provider: config.Provider{Name: "openai"}},
		{name: "OpenAI gateway", provider: config.Provider{Name: "openai", BaseURL: "https://gateway.example.com/v1"}},
		{name: "Bedrock", provider: config.Provider{Name: "amazon-bedrock"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertPinnedCodexAcceptsConfig(t, executable, test.provider)
		})
	}
}

func assertPinnedCodexAcceptsConfig(t *testing.T, executable string, provider config.Provider) {
	t.Helper()
	durable := filepath.Join(t.TempDir(), "data")
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = provider
	cfg.Telemetry = &config.Telemetry{
		CaptureContent: true,
		Traces:         &config.OTLPExporter{Endpoint: "http://collector:4318/v1/traces", Protocol: "http/protobuf"},
		Logs:           &config.OTLPExporter{Endpoint: "http://collector:4318/v1/logs", Protocol: "http/protobuf"},
	}
	cfg.Agents = map[string]config.Agent{"reviewer": {Description: "Reviews", Instruction: "Review", Model: "gpt-5.2-codex"}}
	cfg.MCPServers = map[string]config.MCPServer{"tools": {URL: "https://mcp.example.com/mcp", EnabledTools: []string{"read"}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Input{ConfigJSON: raw, Workspace: filepath.Join(durable, "workspace"), DurableDir: durable, Environment: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(durable, "codex")
	environment := append(os.Environ(), "CODEX_HOME="+codexHome, "OPENAI_API_KEY=test", "AWS_REGION=us-east-1", "AWS_BEARER_TOKEN_BEDROCK=test")
	featuresCommand := exec.Command(executable, "features", "list")
	featuresCommand.Env = environment
	featuresOutput, err := featuresCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("list features from generated Codex config: %v: %s", err, featuresOutput)
	}
	if !featureEnabled(featuresOutput, "default_mode_request_user_input") {
		t.Fatalf("default-mode request_user_input feature is not enabled:\n%s", featuresOutput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "app-server", "--strict-config", "--stdio")
	command.Env = environment
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"kagent-test","version":"1"}}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("initialize generated Codex config: %v: %s", err, stderr.String())
	}
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 1 || len(response.Error) != 0 || len(response.Result) == 0 {
		t.Fatalf("initialize response = %s, stderr = %s", line, stderr.String())
	}
	_ = stdin.Close()
	_ = command.Process.Kill()
	_ = command.Wait()
}

func featureEnabled(output []byte, name string) bool {
	for line := range strings.Lines(string(output)) {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == name && fields[len(fields)-1] == "true" {
			return true
		}
	}
	return false
}

func TestNativeEnvironmentDropsCompressionCodexCannotLoad(t *testing.T) {
	got := nativeEnvironment([]string{"PATH=/bin", "OTEL_EXPORTER_OTLP_COMPRESSION=gzip", "CODEX_HOME=/wrong"}, "/data/codex",
		tracing.RuntimeTelemetry{Runtime: tracing.RuntimeCodex, AgentName: "demo-codex", AgentNamespace: "team"})
	for _, variable := range got {
		if strings.HasPrefix(variable, "OTEL_EXPORTER_OTLP_COMPRESSION=") {
			t.Fatalf("environment = %v, want no OTLP compression", got)
		}
	}
	if !slices.Contains(got, "CODEX_HOME=/data/codex") || !slices.ContainsFunc(got, func(variable string) bool {
		return strings.HasPrefix(variable, "OTEL_RESOURCE_ATTRIBUTES=") && strings.Contains(variable, "gen_ai.agent.name=demo-codex")
	}) {
		t.Fatalf("environment = %v, want CODEX_HOME and the compiled identity", got)
	}
}

func TestRenderConfigAutoCompactTokenLimit(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int64
		want  int64
	}{
		{name: "unset keeps the Codex default"},
		{name: "set", limit: 120000, want: 120000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Production("gpt-5.2-codex", "work carefully")
			cfg.Provider = config.Provider{Name: "openai"}
			cfg.AutoCompactTokenLimit = tt.limit
			contents, err := renderConfig(cfg, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(contents), "model_auto_compact_token_limit"); got != (tt.limit != 0) {
				t.Fatalf("model_auto_compact_token_limit present = %v in:\n%s", got, contents)
			}
			var native nativeConfig
			if err := toml.Unmarshal(contents, &native); err != nil {
				t.Fatal(err)
			}
			if native.AutoCompactLimit != tt.want {
				t.Fatalf("auto compact limit = %d, want %d", native.AutoCompactLimit, tt.want)
			}
		})
	}
}
