package controller

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnvironmentPreparationIsolation(t *testing.T) {
	for _, provider := range []translator.HarnessType{translator.HarnessTypeClaude, translator.HarnessTypeCodex} {
		t.Run(string(provider), func(t *testing.T) {
			stop := make(chan struct{})
			t.Cleanup(func() { close(stop) })
			opts := krt.NewOptionsBuilder(stop, "env-test", nil)
			var config []byte
			var err error
			if provider == translator.HarnessTypeClaude {
				config, err = json.Marshal(claudeconfig.Production("model", "help"))
			} else {
				cfg := codexconfig.Production("model", "help")
				cfg.Provider = codexconfig.Provider{Name: "openai", BaseURL: "https://api.openai.com/v1"}
				config, err = json.Marshal(cfg)
			}
			require.NoError(t, err)
			revision := translator.Revision{Namespace: "agents", AgentName: "writer", AgentUID: "agent-uid", NativeProvider: provider, Image: "legacy", WorkerPoolName: "native", ConfigJSON: config, AgentCard: &a2apb.AgentCard{Name: "writer", Version: "v1", Capabilities: &a2apb.AgentCapabilities{}, SupportedInterfaces: []*a2apb.AgentInterface{{Url: "http://localhost:80", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"}}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}}}
			id, err := revision.Digest()
			require.NoError(t, err)
			original := AgentReconciliation{Agent: &v1alpha3.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "writer", UID: "agent-uid"}}, Target: &compiledTarget{Revision: revision, RevisionID: id}}
			collection := krt.NewStaticCollection(nil, []AgentReconciliation{original}, opts.WithName("Reconciliations")...)
			store := &fakeRuntimeRevisionStore{}
			templates := &fakeActorTemplates{}
			catalogJSON, err := json.Marshal(RuntimePayloadCatalog{string(provider) + "/linux/arm64": {Image: "registry/runtime@sha256:" + strings.Repeat("b", 64), CLIVersion: payload.LockedRelease(string(provider)).Version}})
			require.NoError(t, err)
			catalog, err := ParseRuntimePayloadCatalog(string(catalogJSON))
			require.NoError(t, err)
			pool := &atev1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "native"}, Spec: atev1alpha1.WorkerPoolSpec{Template: &atev1alpha1.WorkerPoolPodTemplate{NodeSelector: map[string]string{corev1.LabelArchStable: "arm64"}}}}
			pools := krt.NewStaticCollection(nil, []*atev1alpha1.WorkerPool{pool}, opts.WithName("WorkerPools")...)
			preparer := NewEnvironmentPreparer(Collections{Reconciliations: collection, WorkerPools: pools}, store, templates, catalog)
			selection := &apiv1alpha1.DevelopmentEnvironment{Image: "registry/dev@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", PolicyIdentity: "accepted-v1"}
			ref := &apiv1alpha1.ResourceReference{Namespace: "agents", Name: "writer"}
			_, _, err = preparer.Prepare(t.Context(), ref, selection)
			require.ErrorContains(t, err, "pending")
			require.NotNil(t, store.revision, "pending candidate must be retained before Session reservation")
			require.False(t, store.markedSuccessful)
			first := store.revision.Revision
			templates.template.Status = &ateapipb.ActorTemplateStatus{GoldenSnapshotStatus: &ateapipb.GoldenSnapshotStatus{GoldenTag: &ateapipb.ObjectRef{Name: "golden", Atespace: "goldens"}}}
			prepared, composition, err := preparer.Prepare(t.Context(), ref, selection)
			require.NoError(t, err)
			require.Equal(t, first, prepared)
			require.Equal(t, string(provider), composition.Provider)
			require.True(t, store.markedSuccessful)
			require.Nil(t, store.pair)
			require.Zero(t, store.pairCalls)
			require.Equal(t, id, collection.GetKey("agents/writer").Target.RevisionID)
			var snapshot database.EnvironmentRevisionSnapshot
			require.NoError(t, json.Unmarshal(store.revision.SourceSnapshot, &snapshot))
			require.Equal(t, id.String(), snapshot.BaseRevision)
			require.True(t, proto.Equal(selection, snapshot.Environment))
			templates.template.Status.GoldenSnapshotStatus.ErrorMessage = "immutable failure"
			_, _, err = preparer.Prepare(t.Context(), ref, selection)
			require.ErrorContains(t, err, "failed")
			templates.template.Status.GoldenSnapshotStatus.ErrorMessage = ""
			selection.Image = "registry/dev@sha256:" + strings.Repeat("c", 64)
			templates.template = nil
			_, _, err = preparer.Prepare(t.Context(), ref, selection)
			require.ErrorContains(t, err, "pending")
			require.NotEqual(t, first, store.revision.Revision)
			// The same digests may represent OCI indexes. Cataloging them under
			// both platforms must not admit amd64 execution on the arm64 pool.
			preparer.catalog[string(provider)+"/linux/amd64"] = preparer.catalog[string(provider)+"/linux/arm64"]
			selection.Platform = "linux/amd64"
			store.revision = nil
			templates.template = nil
			templates.getErr = errors.New("runtime must not be contacted")
			_, _, err = preparer.Prepare(t.Context(), ref, selection)
			require.ErrorContains(t, err, "node selectors")
			require.Nil(t, store.revision)
			require.Nil(t, templates.template)
		})
	}
}

func TestRuntimePayloadCatalogValidation(t *testing.T) {
	for _, raw := range []string{
		`{"claude/linux/arm64":{"image":"image:latest","cliVersion":"2.1.260"}}`,
		`{"unknown/linux/arm64":{"image":"image@sha256:` + strings.Repeat("a", 64) + `","cliVersion":"2.1.260"}}`,
		`{"claude/linux/mips":{"image":"image@sha256:` + strings.Repeat("a", 64) + `","cliVersion":"2.1.260"}}`,
		`{"claude/linux/arm64":{"image":"image@sha256:` + strings.Repeat("a", 64) + `"}}`,
	} {
		_, err := ParseRuntimePayloadCatalog(raw)
		require.Error(t, err)
	}
}

func TestComposedWorkerPoolPlatform(t *testing.T) {
	for _, test := range []struct {
		name, platform, arch, os              string
		missingPool, missingTemplate, allowed bool
	}{
		{name: "arm64", platform: "linux/arm64", arch: "arm64", allowed: true},
		{name: "amd64", platform: "linux/amd64", arch: "amd64", os: "linux", allowed: true},
		{name: "multiarch pool", platform: "linux/arm64"},
		{name: "wrong architecture", platform: "linux/arm64", arch: "amd64"},
		{name: "wrong OS", platform: "linux/arm64", arch: "arm64", os: "windows"},
		{name: "missing pool", platform: "linux/arm64", missingPool: true},
		{name: "missing template", platform: "linux/arm64", missingTemplate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := &atev1alpha1.WorkerPool{Spec: atev1alpha1.WorkerPoolSpec{Template: &atev1alpha1.WorkerPoolPodTemplate{NodeSelector: map[string]string{corev1.LabelArchStable: test.arch, corev1.LabelOSStable: test.os}}}}
			if test.missingPool {
				pool = nil
			} else if test.missingTemplate {
				pool.Spec.Template = nil
			}
			require.Equal(t, test.allowed, validateWorkerPoolPlatform(pool, test.platform) == nil)
		})
	}
}
