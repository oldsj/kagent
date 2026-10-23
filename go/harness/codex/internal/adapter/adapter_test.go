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

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
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
	contents, err := renderConfig(cfg, t.TempDir(), filepath.Join(t.TempDir(), "workspace"))
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
		contents, err := renderConfig(cfg, t.TempDir(), filepath.Join(t.TempDir(), "workspace"))
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
			contents, err := renderConfig(cfg, t.TempDir(), filepath.Join(t.TempDir(), "workspace"))
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

// TestPinnedCodexLeavesTrustedWorkspaceUnchanged drives the pinned Codex CLI's
// own app-server through initialize and thread/start in the generated
// workspace, the exact sequence ProcessDriver.Run makes on turn 1, and checks
// that config.toml comes out byte-identical: proof, against the real binary
// rather than a recorded fixture, that rendering the trust entry up front
// matches what Codex itself would otherwise append.
func TestPinnedCodexLeavesTrustedWorkspaceUnchanged(t *testing.T) {
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("pinned Codex CLI is not installed")
	}
	durable := filepath.Join(t.TempDir(), "data")
	workspaceDir := filepath.Join(durable, "workspace")
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable, Environment: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(durable, "codex", "config.toml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "app-server", "--strict-config", "--stdio")
	command.Dir = workspaceDir
	command.Env = append(os.Environ(), "CODEX_HOME="+filepath.Join(durable, "codex"), "OPENAI_API_KEY=test")
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
	reader := bufio.NewReader(stdout)
	readResponse := func(id int) json.RawMessage {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read Codex app-server response for id %d: %v: %s", id, err, stderr.String())
			}
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatalf("decode Codex app-server response: %v: %s", err, line)
			}
			if response.ID != id {
				continue // an async notification such as remoteControl/status/changed
			}
			if len(response.Error) != 0 {
				t.Fatalf("Codex app-server returned an error for id %d: %s", id, response.Error)
			}
			return response.Result
		}
	}
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"kagent-test","version":"1"},"capabilities":{"experimentalApi":true}}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	readResponse(1)
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	start, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "thread/start",
		"params": map[string]any{
			"cwd": workspaceDir, "model": cfg.Model, "modelProvider": "kagent-openai",
			"approvalPolicy": map[string]any{"granular": map[string]bool{
				"sandbox_approval": false, "rules": false, "skill_approval": false, "request_permissions": false, "mcp_elicitations": true,
			}},
			"sandbox": "danger-full-access", "developerInstructions": cfg.DeveloperInstruction,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(append(start, '\n')); err != nil {
		t.Fatal(err)
	}
	readResponse(2)
	_ = stdin.Close()
	_ = command.Process.Kill()
	_ = command.Wait()

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Codex changed config.toml after thread/start in an already-trusted workspace:\nbefore:\n%s\nafter:\n%s", before, after)
	}
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
			contents, err := renderConfig(cfg, t.TempDir(), filepath.Join(t.TempDir(), "workspace"))
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

// TestRenderConfigAppendsWorkspaceTrust pins the exact bytes the pinned Codex
// CLI 0.148.0 was observed to append to config.toml the first time thread/start
// runs in an untrusted cwd (verified by driving its app-server directly). The
// comparison in SetupInstaller.Setup's observe path is byte-exact, so a
// mismatch here, however small, reproduces mainloop#321.
func TestRenderConfigAppendsWorkspaceTrust(t *testing.T) {
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	contents, err := renderConfig(cfg, t.TempDir(), "/data/workspace")
	if err != nil {
		t.Fatal(err)
	}
	const want = "\n[projects.\"/data/workspace\"]\ntrust_level = \"trusted\"\n"
	if !strings.HasSuffix(string(contents), want) {
		t.Fatalf("generated Codex configuration does not end with Codex's workspace trust entry:\n%s", contents)
	}
}

// codexGitEnabledConfig returns a compiler-owned config whose Git policy and
// MCP server satisfy workspace.ConfigDigests, the shape every Preparation
// Setup call requires regardless of this fix.
func codexGitEnabledConfig(model string) config.Config {
	cfg := config.Production(model, "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	readOrigin, pushOrigin := apiworkspace.GitInstanceOrigins("mainloop")
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin}
	cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: apiworkspace.MCPInstanceOrigin("mainloop") + "/mcp"}}
	return cfg
}

// preparationFixture builds a SetupInstaller and a matching assignment for the
// install/observe round trip the following tests drive directly, the same
// calls executor.New and bootstrap.go's preparationAdmission make each turn.
func preparationFixture(t *testing.T, workspaceDir string) (*SetupInstaller, workspace.Preparation, []byte) {
	t.Helper()
	durable := filepath.Dir(workspaceDir)
	raw, err := json.Marshal(codexGitEnabledConfig("gpt-5.2-codex"))
	require.NoError(t, err)
	store, err := continuation.New(filepath.Join(durable, "adapter"), "codex", func(string) error { return nil })
	require.NoError(t, err)
	configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
	require.NoError(t, err)
	setupDigest, err := workspace.SetupDigest("agent")
	require.NoError(t, err)
	assignment := workspace.Preparation{
		Provider: "codex", Profile: "agent",
		SetupDigest: setupDigest, ConfigDigest: configDigest, MCPDigest: mcpDigest,
	}
	installer := &SetupInstaller{Input: Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable}, Store: store}
	return installer, assignment, raw
}

// TestSetupObservePassesOnceCodexTrustsTheWorkspace reproduces mainloop#321's
// turn 2: turn 1's install leaves config.toml already marking the workspace
// trusted, Codex's own thread/start therefore leaves the file untouched (the
// pinned CLI was observed to do exactly that), and the next observe - the same
// call bootstrap.go's preparationAdmission and executor.New's restart path
// make - must keep passing rather than reporting "Workspace preparation is
// incomplete."
func TestSetupObservePassesOnceCodexTrustsTheWorkspace(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	installer, assignment, _ := preparationFixture(t, workspaceDir)

	_, _, err := installer.Setup(t.Context(), assignment, false)
	require.NoError(t, err)

	configPath := filepath.Join(installer.Input.DurableDir, "codex", "config.toml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)

	// Turn 1's Codex process leaves an already-trusted project's config.toml
	// byte-for-byte unchanged, so nothing touches configPath here.

	_, _, err = installer.Setup(t.Context(), assignment, true)
	require.NoError(t, err, "turn 2's observe must pass once Codex trusts the workspace")

	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// executor.New's restart path runs this exact observe call before it
	// re-renders config.toml; a second observe call models that restart.
	_, _, err = installer.Setup(t.Context(), assignment, true)
	require.NoError(t, err, "a restart's observe must also pass")
}

// TestSetupObservePassesOnSessionsStuckBeforeThisFix answers whether sessions
// already stuck on today's harness self-heal once this fix rolls out and
// restarts them. Today's installed config.toml holds the pre-fix render plus
// whatever Codex's thread/start appended live; since that append is byte-for-
// byte the literal TestRenderConfigAppendsWorkspaceTrust pins, a stuck
// session's on-disk file already equals this fix's renderConfig output, and
// its next observe passes with no migration step.
func TestSetupObservePassesOnSessionsStuckBeforeThisFix(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	durable := filepath.Dir(workspaceDir)
	raw, err := json.Marshal(codexGitEnabledConfig("gpt-5.2-codex"))
	require.NoError(t, err)
	parsed, err := config.Parse(raw)
	require.NoError(t, err)

	codexHome := filepath.Join(durable, "codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	preFixRendered, err := renderConfig(parsed, codexHome, workspaceDir)
	require.NoError(t, err)
	preFixRendered = bytes.TrimSuffix(preFixRendered, workspaceTrustEntry(workspaceDir))
	stuckOnDisk := append(append([]byte{}, preFixRendered...), workspaceTrustEntry(workspaceDir)...)
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), stuckOnDisk, 0o600))

	stateDir := filepath.Join(durable, ".kagent")
	configDigest, mcpDigest, err := workspace.ConfigDigests(raw)
	require.NoError(t, err)
	setupDigest, err := workspace.SetupDigest("agent")
	require.NoError(t, err)
	assignment := workspace.Preparation{Provider: "codex", Profile: "agent", SetupDigest: setupDigest, ConfigDigest: configDigest, MCPDigest: mcpDigest}
	// Records native-setup.json, the way the session's original, pre-fix
	// install already did; unaffected by this fix.
	require.NoError(t, workspace.CheckSetup(raw, stateDir, assignment, false))

	installer := &SetupInstaller{Input: Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable}}
	_, _, err = installer.Setup(t.Context(), assignment, true)
	require.NoError(t, err, "a session stuck on today's harness must pass observe once this fix is rolled out, with no migration")
}

// TestSetupObserveFailsOnDriftOutsideWorkspaceTrust guards against a
// comparison loosened to tolerate the trust block also tolerating unrelated
// drift: observe must still fail closed on any other installed-configuration
// change.
func TestSetupObserveFailsOnDriftOutsideWorkspaceTrust(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	installer, assignment, _ := preparationFixture(t, workspaceDir)

	_, _, err := installer.Setup(t.Context(), assignment, false)
	require.NoError(t, err)

	configPath := filepath.Join(installer.Input.DurableDir, "codex", "config.toml")
	original, err := os.ReadFile(configPath)
	require.NoError(t, err)
	drifted := append(append([]byte{}, original...), []byte("\nweb_search = \"disabled\"\n")...)
	require.NoError(t, os.WriteFile(configPath, drifted, 0o600))

	_, _, err = installer.Setup(t.Context(), assignment, true)
	require.Error(t, err)
}
