package substrate

import (
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSnapshotConfig(t *testing.T) {
	for _, tt := range []struct {
		name         string
		onQuiesce    v1alpha3.RuntimeSnapshotScope
		wantOnCommit ateapipb.SnapshotContentScope
		wantError    string
	}{
		{name: "default is data", wantOnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA},
		{name: "data", onQuiesce: v1alpha3.RuntimeSnapshotScopeData, wantOnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA},
		{name: "full", onQuiesce: v1alpha3.RuntimeSnapshotScopeFull, wantOnCommit: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL},
		{name: "unsupported", onQuiesce: "Memory", wantError: `unsupported quiesce snapshot scope "Memory"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config, err := snapshotConfig("s3://snapshots/", tt.onQuiesce)
			if tt.wantError != "" {
				require.EqualError(t, err, tt.wantError)
				require.Nil(t, config)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "s3://snapshots/", config.GetStorageLocation())
			require.Equal(t, tt.wantOnCommit, config.GetOnCommit())
			// Only the quiesce scope is configurable; waiting turns and resume are fixed.
			require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, config.GetOnPause())
			require.Equal(t, ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN, config.GetOnResume().GetFromData())
		})
	}
}

func TestActorTemplateAppliesQuiesceSnapshotScope(t *testing.T) {
	spec := &translator.Revision{
		Namespace: "agents", AgentName: "helper", WorkerPoolName: "pool", SnapshotLocation: "snapshots", SnapshotOnQuiesce: v1alpha3.RuntimeSnapshotScopeFull,
		AgentCard: &a2apb.AgentCard{Name: "helper", Version: "v1", Capabilities: &a2apb.AgentCapabilities{Streaming: new(true)},
			SupportedInterfaces: []*a2apb.AgentInterface{{Url: "http://127.0.0.1:80", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"}}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}},
	}
	revisionID, err := spec.Digest()
	require.NoError(t, err)
	template, err := ActorTemplateForRevision(spec, revisionID)
	require.NoError(t, err)
	require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, template.GetSnapshotConfig().GetOnCommit())
	require.Equal(t, "snapshots", template.GetSnapshotConfig().GetStorageLocation())

	spec.SnapshotOnQuiesce = "Memory"
	template, err = ActorTemplateForRevision(spec, revisionID)
	require.ErrorContains(t, err, "unsupported quiesce snapshot scope")
	require.Nil(t, template)
}

func TestSandboxTemplateAppliesQuiesceSnapshotScope(t *testing.T) {
	template := &v1alpha3.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "scratch", UID: "uid"}, Spec: v1alpha3.SandboxTemplateSpec{
		Workload:  v1alpha3.SandboxTemplateWorkload{Image: "tools@sha256:" + strings.Repeat("a", 64)},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}, SnapshotPolicy: v1alpha3.RuntimeSnapshotPolicy{Location: "s3://snapshots/"}},
	}}
	policy := SandboxPolicy{GuestImage: "guest@sha256:" + strings.Repeat("b", 64), CPU: "1", Memory: "1Gi"}
	actor, defaultDigest, _, err := SandboxActorTemplate(template, "", policy)
	require.NoError(t, err)
	require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA, actor.GetSnapshotConfig().GetOnCommit())

	template.Spec.Substrate.SnapshotPolicy.OnQuiesce = v1alpha3.RuntimeSnapshotScopeFull
	actor, fullDigest, _, err := SandboxActorTemplate(template, "", policy)
	require.NoError(t, err)
	require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL, actor.GetSnapshotConfig().GetOnCommit())
	require.NotEqual(t, defaultDigest, fullDigest)
}
