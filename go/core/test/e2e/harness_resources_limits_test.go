package e2e_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/metadata"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
)

// TestHarnessResourcesSizeEachHarness checks the reason spec.resources exists:
// two Harnesses in one installation give their Agents differently sized
// runtimes. It reads the limits back from Substrate, where they take effect.
func TestHarnessResourcesSizeEachHarness(t *testing.T) {
	target := interactionTarget(t)
	kube := interactionKubeClient(t)
	system := apiv1.NewSystemServiceClient(newControllerConn(t, target))
	harness := testHarness{name: claudeE2EHarness, runtimeLabel: "claude"}
	model := harness.createModel(t, kube, startInteractionMock(t), nil)

	t.Run("Given two Harnesses with different CPU and memory limits, when an Agent becomes ready on each, assert that Substrate holds each Agent's own limits", func(t *testing.T) {
		small := readyAgentOnSizedHarness(t, kube, harness, model.Name, "500m", "768Mi")
		large := readyAgentOnSizedHarness(t, kube, harness, model.Name, "1500m", "1536Mi")

		templates := actorTemplatesByAgent(t, system, small, large)

		smallLimits := templates[small.Name].GetResources().GetLimits()
		require.Equal(t, int64(500), smallLimits.GetCpuMillicores())
		require.Equal(t, int64(768<<20), smallLimits.GetMemoryBytes())
		largeLimits := templates[large.Name].GetResources().GetLimits()
		require.Equal(t, int64(1500), largeLimits.GetCpuMillicores())
		require.Equal(t, int64(1536<<20), largeLimits.GetMemoryBytes())
	})
}

// readyAgentOnSizedHarness binds a new Agent to a copy of the harness under
// test with the given limits and waits for its revision to be prepared.
func readyAgentOnSizedHarness(t *testing.T, kube ctrlclient.Client, harness testHarness, modelName, cpu, memory string) *v1alpha3.Agent {
	t.Helper()
	source := &v1alpha3.Harness{}
	require.NoError(t, kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: harness.name}, source))
	sized := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "harness-limits-", Namespace: source.Namespace, Labels: harness.labels()},
		Spec:       *source.Spec.DeepCopy(),
	}
	sized.Spec.Resources = &v1alpha3.HarnessResources{CPU: new(resource.MustParse(cpu)), Memory: new(resource.MustParse(memory))}
	require.NoError(t, kube.Create(t.Context(), sized))
	agent := &v1alpha3.Agent{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "harness-limits-", Namespace: source.Namespace, Labels: harness.labels()},
		Spec: v1alpha3.AgentSpec{
			HarnessRef: corev1.LocalObjectReference{Name: sized.Name},
			Template:   v1alpha3.AgentTemplateSource{Inline: &v1alpha3.AgentTemplateSpec{ModelConfig: &v1alpha3.ObjectReference{Name: modelName}, SystemPrompt: "Be helpful."}},
		},
	}
	require.NoError(t, kube.Create(t.Context(), agent))
	t.Cleanup(func() {
		for _, object := range []ctrlclient.Object{agent, sized} {
			if err := kube.Delete(context.Background(), object); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete %T %s: %v", object, object.GetName(), err)
			}
		}
	})
	waitForAgentRevision(t, kube, agent)
	return agent
}

// actorTemplatesByAgent returns the ActorTemplate Substrate holds for each
// Agent. A template is named for its Agent and revision.
func actorTemplatesByAgent(t *testing.T, system apiv1.SystemServiceClient, agents ...*v1alpha3.Agent) map[string]*ateapipb.ActorTemplate {
	t.Helper()
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-user-id", "e2e")
	summary, err := system.GetSubstrateSummary(ctx, &apiv1.GetSubstrateSummaryRequest{Namespace: "kagent", Atespace: "kagent"})
	require.NoError(t, err)
	require.Empty(t, summary.GetAteApiError())
	found := map[string]*ateapipb.ActorTemplate{}
	for _, agent := range agents {
		for _, template := range summary.GetActorTemplates() {
			if strings.HasPrefix(template.GetMetadata().GetName(), agent.Name+"-") {
				found[agent.Name] = template
			}
		}
		require.Contains(t, found, agent.Name, "Substrate has no ActorTemplate for Agent %s", agent.Name)
	}
	return found
}
