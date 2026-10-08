package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/krt/krttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const credentialValue = "credential-must-not-be-serialized"

func TestCompileProviderCredentials(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	tests := []struct {
		name        string
		model       v1alpha3.ModelConfigSpec
		secret      map[string][]byte
		provider    string
		baseURL     string
		environment map[string]string
		egress      []string
		wantErr     string
	}{
		{
			name: "ChatGPT subscription", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-6.1", APIKeySecret: "model-auth", APIKeySecretKey: "access-token", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses, AuthMethod: v1alpha3.OpenAIAuthMethod_ChatGPT, AccountID: "test-account"}},
			secret: map[string][]byte{"access-token": []byte(credentialValue)}, provider: "chatgpt", egress: []string{"http://kagent-controller.kagent:8083", "https://chatgpt.com:443"},
		},
		{
			name: "ChatGPT missing account", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-6.1", APIKeySecret: "model-auth", APIKeySecretKey: "access-token", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses, AuthMethod: v1alpha3.OpenAIAuthMethod_ChatGPT}},
			secret: map[string][]byte{"access-token": []byte(credentialValue)}, wantErr: "requires accountID",
		},
		{
			name: "ChatGPT alternate backend", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-6.1", APIKeySecret: "model-auth", APIKeySecretKey: "access-token", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses, AuthMethod: v1alpha3.OpenAIAuthMethod_ChatGPT, AccountID: "test-account", BaseURL: "https://codex.example.com/backend-api/codex"}},
			secret: map[string][]byte{"access-token": []byte(credentialValue)}, provider: "chatgpt", baseURL: "https://codex.example.com/backend-api/codex", egress: []string{"http://kagent-controller.kagent:8083", "https://codex.example.com:443"},
		},
		{
			name: "OpenAI", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses}},
			secret: map[string][]byte{"api-key": []byte(credentialValue)}, provider: "openai", environment: map[string]string{openAIAPIKeyEnv: v2translator.CredentialPlaceholder}, egress: []string{"http://kagent-controller.kagent:8083", "https://api.openai.com:443"},
		},
		{
			name: "OpenAI gateway", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses, BaseURL: "https://gateway.example.com/v1"}},
			secret: map[string][]byte{"api-key": []byte(credentialValue)}, provider: "openai", environment: map[string]string{openAIAPIKeyEnv: v2translator.CredentialPlaceholder}, egress: []string{"http://kagent-controller.kagent:8083", "https://gateway.example.com:443"},
		},
		{
			name: "Bedrock API key", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderBedrock, Model: "gpt-5.2", APIKeySecret: "model-auth", Bedrock: &v1alpha3.BedrockConfig{Region: "us-east-1", CacheTTL: "5m"}},
			secret: map[string][]byte{awsBedrockTokenEnv: []byte(credentialValue)}, provider: "amazon-bedrock", environment: map[string]string{awsRegionEnv: "us-east-1", awsBedrockTokenEnv: v2translator.CredentialPlaceholder}, egress: []string{"http://kagent-controller.kagent:8083", "https://bedrock-runtime.us-east-1.amazonaws.com:443"},
		},
		{
			name: "Bedrock IAM", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderBedrock, Model: "gpt-5.2", APIKeySecret: "model-auth", Bedrock: &v1alpha3.BedrockConfig{Region: "us-west-2"}},
			secret: map[string][]byte{awsAccessKeyEnv: []byte("access"), awsSecretKeyEnv: []byte(credentialValue), awsSessionTokenEnv: []byte("session")}, wantErr: "cannot use gateway header injection",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, reader := testInput(t, test.model, test.secret)
			revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected unsupported credential error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var cfg codexconfig.Config
			if err := json.Unmarshal(revision.ConfigJSON, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Provider.Name != test.provider || cfg.ExpectedCodexVersion != codexconfig.PinnedCodexVersion {
				t.Fatalf("config = %#v", cfg)
			}
			gotEnvironment := map[string]string{}
			for _, variable := range revision.Environment {
				if variable.ValueFrom != nil {
					t.Fatalf("unresolved environment = %#v", variable)
				}
				gotEnvironment[variable.Name] = variable.Value
			}
			for name, value := range test.environment {
				if gotEnvironment[name] != value {
					t.Errorf("environment[%s] = %q, want %q", name, gotEnvironment[name], value)
				}
			}
			if test.provider == "chatgpt" {
				if _, ok := gotEnvironment[openAIAPIKeyEnv]; ok {
					t.Fatal("ChatGPT Actor has an API-key environment variable")
				}
				if cfg.Provider.AccountID != "test-account" || len(revision.Credentials) != 1 {
					t.Fatal("missing ChatGPT account or credential binding")
				}
				binding, wantHost := revision.Credentials[0], "chatgpt.com"
				if test.baseURL != "" {
					wantHost = "codex.example.com"
				}
				if cfg.Provider.BaseURL != test.baseURL || binding.Hostname != wantHost || binding.Header != "authorization" || binding.Prefix != "Bearer " || binding.URI != "ate-secret://k8s.io/default/test/model-auth/access-token" {
					t.Fatalf("incorrect ChatGPT binding: %#v", binding)
				}
			}
			if !reflect.DeepEqual(revision.EgressDestinations, test.egress) {
				t.Errorf("egress = %v, want %v", revision.EgressDestinations, test.egress)
			}
			if bytes.Contains(revision.ConfigJSON, []byte(credentialValue)) || bytes.Contains(revision.Provenance, []byte(credentialValue)) {
				t.Fatal("credential leaked into immutable revision")
			}
			again, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
			if err != nil || !reflect.DeepEqual(revision, again) {
				t.Fatalf("compilation is not deterministic: %v", err)
			}
		})
	}
}

func TestCompileTracing(t *testing.T) {
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "NO_CONTENT")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://logs:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "grpc")
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var cfg codexconfig.Config
	if err := json.Unmarshal(revision.ConfigJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry == nil || cfg.Telemetry.Traces == nil || cfg.Telemetry.Traces.Endpoint != "http://collector:4318/v1/traces" || cfg.Telemetry.Traces.Protocol != "http/protobuf" || cfg.Telemetry.Logs == nil || cfg.Telemetry.Logs.Endpoint != "http://logs:4317" || cfg.Telemetry.Logs.Protocol != "grpc" || cfg.Telemetry.CaptureContent {
		t.Fatalf("telemetry = %#v", cfg.Telemetry)
	}
	if !reflect.DeepEqual(revision.EgressDestinations, []string{"http://collector:4318", "http://kagent-controller.kagent:8083", "http://logs:4317", "https://api.openai.com:443"}) {
		t.Fatalf("egress = %v", revision.EgressDestinations)
	}
	environment := map[string]string{}
	for _, variable := range revision.Environment {
		environment[variable.Name] = variable.Value
	}
	for name, value := range map[string]string{
		"OTEL_TRACES_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "none", "OTEL_LOGS_EXPORTER": "otlp",
		"OTEL_EXPORTER_OTLP_ENDPOINT":      "http://collector:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL":      "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://logs:4317",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc",
		"OTEL_SERVICE_NAME":                "runnable-agent",
		"KAGENT_NAME":                      "runnable-agent",
		"KAGENT_NAMESPACE":                 "test",
	} {
		if environment[name] != value {
			t.Errorf("environment[%s] = %q, want %q", name, environment[name], value)
		}
	}

	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "SPAN_ONLY")
	revision, err = NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(revision.ConfigJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry == nil || !cfg.Telemetry.CaptureContent {
		t.Fatalf("sensitive-content telemetry = %#v", cfg.Telemetry)
	}
}

func TestCompileRejectsManagedOTELEnvironment(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	value := "http://other-collector:4317"
	input.Harness.Spec.Env = []v1alpha3.RuntimeEnvVar{{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: value}}

	_, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	var validation *v2translator.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), "conflicts with Codex's compiled configuration") {
		t.Fatalf("Compile() error = %v, want managed OTEL environment conflict", err)
	}
}

func TestCompileAllowsUnmanagedOTELEnvironment(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	value := "x-tenant=team-a"
	input.Harness.Spec.Env = []v1alpha3.RuntimeEnvVar{{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: value}}

	revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(revision.Environment, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: value}) {
		t.Fatalf("unmanaged OTEL environment missing from revision: %#v", revision.Environment)
	}
}

func TestCompileRejectsUnsupportedProviderConfiguration(t *testing.T) {
	responses, chat := v1alpha3.OpenAIAPIFormatResponses, v1alpha3.OpenAIAPIFormatChatCompletions
	tests := []v1alpha3.ModelConfigSpec{
		{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude"},
		{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &chat}},
		{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses, Temperature: "1"}},
		{Provider: v1alpha3.ModelProviderBedrock, Model: "gpt", APIKeySecret: "model-auth", Bedrock: &v1alpha3.BedrockConfig{Region: "us-east-1", PromptCaching: true}},
	}
	for _, model := range tests {
		input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret"), awsAccessKeyEnv: []byte("access"), awsSecretKeyEnv: []byte("secret")})
		_, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
		var validation *v2translator.ValidationError
		if !errors.As(err, &validation) {
			t.Errorf("Compile(%s) error = %v, want validation", model.Provider, err)
		}
	}
}

func TestCompileMCPAndSharedAgent(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-root", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses}}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret"), "mcp-token": []byte(credentialValue)})
	server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "tools", Namespace: "test", UID: "mcp"}, Spec: v1alpha3.RemoteMCPServerSpec{
		Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp, URL: "https://mcp.example.com/mcp", HeadersFrom: []v1alpha3.ValueRef{{Name: "Authorization", ValueFrom: &v1alpha3.ValueSource{Type: v1alpha3.SecretValueSource, Name: "model-auth", Key: "mcp-token"}}},
	}}
	input.Root.MCPTools = []v2translator.ResolvedMCPTool{{Binding: v1alpha3.MCPToolBinding{Tools: []string{"read"}, RequireApproval: true}, Server: server}}
	childModel := model
	childModel.Model = "gpt-child"
	input.Root.Shared = []v2translator.AgentInputBinding{{Name: "reviewer", Description: "Reviews", Agent: &v2translator.AgentInput{
		Template: &v2translator.TemplateConfiguration{Name: "child", Namespace: "test", Source: &metav1.ObjectMeta{Name: "child", Namespace: "test"}, Spec: v1alpha3.AgentTemplateSpec{ModelConfig: &corev1.LocalObjectReference{Name: "child-model"}}},
		ResolvedModelConfig: &v2translator.ResolvedModelConfig{
			Config: &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "child-model", Namespace: "test"}, Spec: childModel},
		},
		Instruction: "Review carefully",
	}}}
	revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(revision.Warnings) != 0 {
		t.Fatalf("MCP compatibility warnings = %v", revision.Warnings)
	}
	var cfg codexconfig.Config
	if err := json.Unmarshal(revision.ConfigJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["reviewer"].Model != "gpt-child" || !cfg.MCPServers["tools"].RequireApproval || !reflect.DeepEqual(cfg.MCPServers["tools"].EnabledTools, []string{"read"}) {
		t.Fatalf("config = %#v", cfg)
	}
	if !strings.HasPrefix(cfg.MCPServers["tools"].Headers["Authorization"], "${"+mcpCredentialPrefix) {
		t.Fatalf("MCP headers = %#v", cfg.MCPServers["tools"].Headers)
	}
	if !reflect.DeepEqual(revision.EgressDestinations, []string{"http://kagent-controller.kagent:8083", "https://api.openai.com:443", "https://mcp.example.com:443"}) {
		t.Fatalf("egress = %v", revision.EgressDestinations)
	}
}

func TestCompileMCPCompatibilityWarnings(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-root",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	terminateOnClose := false
	server := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "tools", Namespace: "test", UID: "mcp"},
		Spec: v1alpha3.RemoteMCPServerSpec{
			Protocol:         v1alpha3.RemoteMCPServerProtocolStreamableHttp,
			URL:              "https://mcp.example.com/mcp",
			TLS:              &v1alpha3.TLSConfig{DisableVerify: true},
			Timeout:          &metav1.Duration{Duration: time.Minute},
			TerminateOnClose: &terminateOnClose,
		},
	}
	input.Root.MCPTools = []v2translator.ResolvedMCPTool{{Binding: v1alpha3.MCPToolBinding{RequireApproval: true}, Server: server}}

	compilation, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(compilation.Warnings) != 1 {
		t.Fatalf("MCP compatibility warnings = %v", compilation.Warnings)
	}
	for _, field := range []string{"custom TLS configuration", "timeout", "terminateOnClose"} {
		if !strings.Contains(compilation.Warnings[0], field) {
			t.Errorf("MCP compatibility warning %q omits %q", compilation.Warnings[0], field)
		}
	}
	var cfg codexconfig.Config
	if err := json.Unmarshal(compilation.ConfigJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	if configured, exists := cfg.MCPServers[server.Name]; !exists || !configured.RequireApproval || len(configured.EnabledTools) != 0 {
		t.Fatalf("config omits MCP server after compatibility warning: %#v", cfg.MCPServers)
	}

	server.Spec.Protocol = v1alpha3.RemoteMCPServerProtocolSse
	if _, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input); err == nil || !strings.Contains(err.Error(), "requires Streamable HTTP") {
		t.Fatalf("unsupported MCP protocol Compile() error = %v", err)
	}
}

func testInput(t *testing.T, modelSpec v1alpha3.ModelConfigSpec, secretData map[string][]byte) (*v2translator.HarnessInput, v2translator.Collections) {
	t.Helper()
	harness := &v2translator.HarnessConfiguration{Name: "codex", Namespace: "test", Source: &metav1.ObjectMeta{Name: "codex", Namespace: "test", UID: "harness"}, Spec: v1alpha3.HarnessSpec{
		Codex: &v1alpha3.CodexHarness{}, Workload: v1alpha3.HarnessWorkload{Image: "example.com/codex@sha256:" + strings.Repeat("a", 64)},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}, SnapshotPolicy: v1alpha3.RuntimeSnapshotPolicy{Location: "snapshots"}},
	}}
	template := &v2translator.TemplateConfiguration{Name: "assistant", Namespace: "test", Source: &metav1.ObjectMeta{Name: "assistant", Namespace: "test", UID: "template"}, Spec: v1alpha3.AgentTemplateSpec{ModelConfig: &corev1.LocalObjectReference{Name: "model"}, Description: "assistant"}}
	model := &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test", UID: "model"}, Spec: modelSpec}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-auth", Namespace: "test", UID: "secret"}, Data: secretData}
	mock := krttest.NewMock(t, []any{secret})
	collections := v2translator.Collections{
		Secrets:    krttest.GetMockCollection[*corev1.Secret](mock),
		ConfigMaps: krttest.GetMockCollection[*corev1.ConfigMap](mock),
	}
	return &v2translator.HarnessInput{AgentName: "runnable-agent", Harness: harness, Root: &v2translator.AgentInput{
		Template: template, ResolvedModelConfig: &v2translator.ResolvedModelConfig{Config: model}, Instruction: "help carefully",
	}}, collections
}

func TestCompileRuntimeTelemetry(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4317")
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	input.Harness.Name = "fast"

	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "NO_CONTENT")
	revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	config, err := codexconfig.Parse(revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	// Changing the Harness name must not change the Agent runtime identity.
	want := tracing.RuntimeTelemetry{
		Runtime: tracing.RuntimeCodex, AgentName: "runnable-agent", AgentNamespace: "test",
		Provider: "openai", Model: "gpt-5.2-codex",
	}
	if config.RuntimeTelemetry != want {
		t.Fatalf("runtime telemetry = %#v, want %#v", config.RuntimeTelemetry, want)
	}
	if config.RuntimeTelemetry.CaptureLimit() != 0 {
		t.Fatal("content capture is not disabled by default")
	}

	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "SPAN_ONLY")
	captured, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	capturedConfig, err := codexconfig.Parse(captured.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !capturedConfig.RuntimeTelemetry.CaptureContent {
		t.Fatalf("captured runtime telemetry = %#v", capturedConfig.RuntimeTelemetry)
	}
	// The native exporter settings stay separate from the Go wrapper contract.
	if capturedConfig.Telemetry == nil || !capturedConfig.Telemetry.CaptureContent {
		t.Fatalf("native telemetry = %#v", capturedConfig.Telemetry)
	}
	// A telemetry change lives only in the compiled configuration, which the
	// revision digest covers. Provenance records Kubernetes inputs, none of
	// which changed.
	if !bytes.Equal(revision.Provenance, captured.Provenance) {
		t.Fatal("changing the capture policy changed revision provenance")
	}
	before, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	after, err := captured.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("changing the capture policy did not change the revision digest")
	}
}

func TestCompileAutoCompactTokenLimit(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	model := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2-codex",
		APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses},
	}
	input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("secret")})
	compile := func() (*v2translator.CompileResult, codexconfig.Config) {
		t.Helper()
		revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		config, err := codexconfig.Parse(revision.ConfigJSON)
		if err != nil {
			t.Fatal(err)
		}
		return revision, config
	}
	unsetRevision, unset := compile()
	if unset.AutoCompactTokenLimit != 0 || strings.Contains(string(unsetRevision.ConfigJSON), "auto_compact") {
		t.Fatalf("unset limit leaked into config: %s", unsetRevision.ConfigJSON)
	}
	unsetID, err := unsetRevision.Digest()
	if err != nil {
		t.Fatal(err)
	}

	input.Harness.Spec.Codex.AutoCompactTokenLimit = new(int64(120000))
	revision, config := compile()
	if config.AutoCompactTokenLimit != 120000 {
		t.Fatalf("auto compact limit = %d, want 120000", config.AutoCompactTokenLimit)
	}
	id, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if id == unsetID {
		t.Fatal("changing the compaction limit must create a new runtime revision")
	}
}

func TestCompileTrustedGitCompleteUnion(t *testing.T) {
	packages := []string{"https://registry.npmjs.org", "https://pypi.org", "https://files.pythonhosted.org", "https://proxy.golang.org", "https://sum.golang.org", "https://storage.googleapis.com"}
	fixture := func() (*v2translator.HarnessInput, v2translator.Collections) {
		input, reader := testInput(t, v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt", APIKeySecret: "model-auth", APIKeySecretKey: "api-key", OpenAI: &v1alpha3.OpenAIConfig{APIFormat: new(v1alpha3.OpenAIAPIFormatResponses)}}, map[string][]byte{"api-key": []byte(credentialValue)})
		input.Harness.Spec.Git = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
		input.Harness.Spec.ExtraHTTPSOrigins = packages
		mock := krttest.NewMock(t, []any{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-auth", Namespace: "test"}, Data: map[string][]byte{"api-key": []byte(credentialValue)}}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mcp-binding", Namespace: "test"}, Data: map[string][]byte{"authorization": []byte(credentialValue)}}})
		reader.Secrets = krttest.GetMockCollection[*corev1.Secret](mock)
		input.Root.MCPTools = []v2translator.ResolvedMCPTool{{Binding: v1alpha3.MCPToolBinding{Server: corev1.TypedLocalObjectReference{Kind: "RemoteMCPServer", Name: "tools"}}, Server: &v1alpha3.RemoteMCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "tools", Namespace: "test", UID: "mcp-uid", Generation: 1},
			Spec:       v1alpha3.RemoteMCPServerSpec{URL: "http://mainloop-mcp.mainloop.svc.cluster.local/mcp", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp, HeadersFrom: []v1alpha3.ValueRef{{Name: "Authorization", ValueFrom: &v1alpha3.ValueSource{Type: v1alpha3.SecretValueSource, Name: "mcp-binding", Key: "authorization"}}}},
			Status:     v1alpha3.RemoteMCPServerStatus{ObservedGeneration: 1, DiscoveredTools: []*v1alpha3.MCPTool{{Name: "echo"}}},
		}}}
		return input, reader
	}
	input, reader := fixture()
	revision, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, []string{"github.com"}, revision.GitOrigins)
	require.Contains(t, revision.EgressDestinations, workspace.ReadProxyOrigin+":80")
	require.Contains(t, revision.EgressDestinations, workspace.PushProxyOrigin+":80")
	require.Contains(t, revision.EgressDestinations, "http://kagent-controller.kagent:8083")
	require.Len(t, revision.Credentials, 2, "only provider and MCP; no revision Git value/reference")
	for _, origin := range packages {
		require.Contains(t, revision.EgressDestinations, origin+":443")
	}
	refs := []*apiv1alpha1.SessionCredential{
		{Origin: workspace.ReadProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-read-unpublished", Key: "authorization"}},
		{Origin: workspace.PushProxyOrigin, Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-push-unpublished", Key: "authorization"}},
	}
	bindings, err := egress.SessionCredentials("test", refs, revision.EgressDestinations, revision.Credentials)
	require.NoError(t, err)
	generation, _, err := substrate.NewRuntimeGeneration("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "test", "test")
	require.NoError(t, err)
	actorPolicy, err := substrate.RuntimeEgressPolicy(generation, "http://kagent-controller.kagent:8083", revision.EgressDestinations, bindings)
	require.NoError(t, err)
	for _, rule := range actorPolicy.Rules {
		for _, host := range rule.GetHttps().GetHostnames() {
			if slices.Contains(packages, "https://"+host) {
				require.Empty(t, rule.GetHttps().GetEffects().GetReplaceHeaders(), host)
			}
		}
	}
	var parsed codexconfig.Config
	require.NoError(t, json.Unmarshal(revision.ConfigJSON, &parsed))
	require.Equal(t, workspace.ReadProxyOrigin, *parsed.Git.ReadProxyOrigin)
	first, err := revision.Digest()
	require.NoError(t, err)
	input.Harness.Spec.Git.PushProxyOrigin = nil
	readonly, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
	require.NoError(t, err)
	second, err := readonly.Digest()
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	for _, noGit := range []bool{false, true} {
		for _, via := range []string{"extra", "MCP", "skill", "provider", "telemetry", "legacy model credential", "reserved MCP credential"} {
			t.Run(fmt.Sprintf("noGit=%t/%s", noGit, via), func(t *testing.T) {
				input, reader := fixture()
				if noGit {
					input.Harness.Spec.Git = nil
				}
				switch via {
				case "extra":
					input.Harness.Spec.ExtraHTTPSOrigins = append(append([]string(nil), packages...), "https://API.GITHUB.COM:443")
				case "MCP":
					input.Root.MCPTools[0].Server.Spec.URL = "https://api.github.com/mcp"
					input.Root.MCPTools[0].Server.Spec.HeadersFrom = nil
				case "skill":
					input.Root.Template.Spec.Skills = []v1alpha3.AgentTemplateSkill{{Name: "review", Source: v1alpha3.ArtifactSource{Git: &v1alpha3.GitArtifact{URL: "https://github.com/o/skills.git", Commit: strings.Repeat("a", 40)}}}}
				case "provider":
					input.Root.ResolvedModelConfig.Config.Spec.OpenAI.BaseURL = "https://api.github.com/v1"
				case "telemetry":
					t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
					t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://github.com:443/v1/traces")
					t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
				case "legacy model credential":
					input.Root.ResolvedModelConfig.Config.Spec.APIKeySecret = "mainloop-git-auth"
					mock := krttest.NewMock(t, []any{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mainloop-git-auth", Namespace: "test"}, Data: map[string][]byte{"api-key": []byte(credentialValue)}}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mcp-binding", Namespace: "test"}, Data: map[string][]byte{"authorization": []byte(credentialValue)}}})
					reader.Secrets = krttest.GetMockCollection[*corev1.Secret](mock)
				case "reserved MCP credential":
					input.Root.MCPTools[0].Server.Spec.URL = workspace.ReadProxyOrigin + "/mcp"
				}
				_, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
				if via == "provider" || via == "legacy model credential" || via == "reserved MCP credential" {
					require.ErrorContains(t, err, "reserved Git credential")
				} else {
					require.ErrorContains(t, err, "direct GitHub authority")
				}
			})
		}
	}
	input, reader = fixture()
	input.Harness.Spec.Git = nil
	input.Root.MCPTools[0].Server.Spec.URL = workspace.ReadProxyOrigin + "/mcp"
	input.Root.MCPTools[0].Server.Spec.HeadersFrom = nil
	_, err = NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
	require.ErrorContains(t, err, "unapproved Git proxy origin")
	input, reader = fixture()
	input.Harness.Spec.Git = nil
	coordinator, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
	require.NoError(t, err)
	require.Empty(t, coordinator.GitOrigins)
	require.NotContains(t, coordinator.EgressDestinations, workspace.ReadProxyOrigin+":80")
	require.NotContains(t, coordinator.EgressDestinations, workspace.PushProxyOrigin+":80")
}
