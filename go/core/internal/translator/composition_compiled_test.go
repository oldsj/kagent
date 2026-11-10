package translator_test

import (
	"testing"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

func TestComposeCompiledRevision(t *testing.T) {
	t.Run("Given a compiled Claude Agent on an arm64 pool, when it is composed with an arm64 selection, assert that composition succeeds and IS_SANDBOX is left to the launcher", func(t *testing.T) {
		harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "claude", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{Claude: &v1alpha3.ClaudeHarness{}}}
		harness.Spec.Substrate.WorkerPoolRef.Name = "arm64"
		template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test"}, Spec: v1alpha3.AgentTemplateSpec{ModelConfig: &v1alpha3.ObjectReference{Name: "default-model"}}}
		model := modelConfig()
		model.Spec.Provider, model.Spec.Model, model.Spec.APIKeySecret, model.Spec.APIKeySecretKey = v1alpha3.ModelProviderAnthropic, "claude-sonnet-4-5", "auth", "key"
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "test"}, Data: map[string][]byte{"key": []byte("secret")}}
		pool := &atev1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "arm64"}}
		pool.Spec.Template = &atev1.WorkerPoolPodTemplate{}
		pool.Spec.Template.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
		compiled, err := compiler(t, model, secret, harness, pool).CompileAgent(t.Context(), inlineAgent(harness, template))
		if err != nil {
			t.Fatalf("CompileAgent() error = %v", err)
		}

		composed, err := translator.ComposeRevision(compiled.Revision, translator.Composition{
			DevelopmentImage: "registry.example/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			PayloadImage:     "registry.example/runtime-claude@sha256:2222222222222222222222222222222222222222222222222222222222222222",
			CLIVersion:       "2.1.285", Platform: "linux/arm64", PolicyIdentity: "project-a/v1", Provider: translator.HarnessTypeClaude, Schema: payload.Schema,
		})

		if err != nil {
			t.Fatalf("ComposeRevision() error = %v", err)
		}
		for _, variable := range composed.Environment {
			if variable.Name == "IS_SANDBOX" {
				t.Fatalf("composed environment keeps %s=%s; the launcher owns it", variable.Name, variable.Value)
			}
		}
	})
}
