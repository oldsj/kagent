package e2e_test

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	adkconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
)

type testHarness struct {
	name         string
	runtimeLabel string
	// bound is the Harness that Agents reference when it is not name: a copy
	// of name with other options, so name's model and mock fixtures still apply.
	bound string
}

// harnessRef is the Harness an Agent of this test harness references.
func (h testHarness) harnessRef() string {
	if h.bound != "" {
		return h.bound
	}
	return h.name
}

// withGit returns the harness bound to a copy of its Harness that lets Sessions
// bootstrap a Git workspace from origins. The copy is deleted with the test.
func (h testHarness) withGit(t *testing.T, kube ctrlclient.Client, origins ...string) testHarness {
	t.Helper()
	source := &v1alpha3.Harness{}
	if err := kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: h.harnessRef()}, source); err != nil {
		t.Fatalf("read Harness %s: %v", h.harnessRef(), err)
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{GenerateName: h.runtimeLabel + "-git-", Namespace: source.Namespace, Labels: h.labels()},
		Spec:       *source.Spec.DeepCopy(),
	}
	harness.Spec.Git = &v1alpha3.HarnessGit{Origins: origins}
	if err := kube.Create(t.Context(), harness); err != nil {
		t.Fatalf("create Git Harness: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), harness); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Git Harness: %v", err)
		}
	})
	h.bound = harness.Name
	return h
}

// Every shared AgentTemplate test runs on every harness type. BYO uses the Go
// ADK image so managed configuration is exercised; the opaque BYO contract has
// its own test. Keep exclusions in the affected test, with an explicit t.Skip
// reason. Never infer exclusions from installed fixtures or readiness failures.
func forEachHarness(t *testing.T, run func(*testing.T, testHarness)) {
	t.Helper()
	for _, harness := range []testHarness{
		{name: "kagent", runtimeLabel: "kagent"},
		{name: codexE2EHarness, runtimeLabel: "codex"},
		{name: claudeE2EHarness, runtimeLabel: "claude"},
		{name: "byo-adk-e2e", runtimeLabel: "byo-adk"},
	} {
		t.Run(harness.runtimeLabel, func(t *testing.T) {
			run(t, harness)
		})
	}
}

func (h testHarness) labels() map[string]string {
	return map[string]string{"kagent.dev/e2e-runtime": h.runtimeLabel, "kagent.dev/harness": h.name}
}

func (h testHarness) createModel(t *testing.T, kube ctrlclient.Client, modelURL string, headers map[string]string) *v1alpha3.ModelConfig {
	t.Helper()
	switch h.name {
	case "kagent", "byo-adk-e2e":
		return createInteractionModel(t, kube, modelURL, headers)
	case codexE2EHarness, claudeE2EHarness:
		if len(headers) != 0 {
			t.Fatalf("%s does not support ModelConfig defaultHeaders", h.name)
		}
		if h.name == codexE2EHarness {
			return createCodexMockModel(t, kube, modelURL)
		}
		// Anthropic clients append /v1 themselves; shared mock URLs include it
		// for OpenAI clients.
		return createClaudeMockModel(t, kube, strings.TrimSuffix(modelURL, "/v1"))
	default:
		t.Fatalf("no model fixture for harness %q", h.name)
		return nil
	}
}

func (h testHarness) skillsRoot() string {
	switch h.name {
	case codexE2EHarness:
		return codexconfig.SkillsRoot("/data")
	case claudeE2EHarness:
		return claudeconfig.SkillsRoot("/data")
	default:
		return adkconfig.DefaultSkillsRoot
	}
}
