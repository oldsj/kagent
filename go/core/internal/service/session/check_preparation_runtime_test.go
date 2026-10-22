package session

import (
	"encoding/json"
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"github.com/stretchr/testify/require"
)

// checkPreparationRuntimeFixture builds the Session, RuntimeRevision, Actor and
// ActorTemplate for a Harness whose Git policy proxies through the named
// Mainloop Git instance (e.g. "mainloop" or "mainloop-dev"), using the same
// translator.CompileGit/ComposeRevision and substrate.ActorTemplateForRevision
// production code the controller uses to build these for the default instance.
func checkPreparationRuntimeFixture(t *testing.T, namespace string) (*api.Session, *database.RuntimeRevision, *ateapipb.Actor, *ateapipb.ActorTemplate) {
	t.Helper()
	readOrigin, pushOrigin := apiworkspace.GitInstanceOrigins(namespace)
	gitPolicy, _, err := translator.CompileGit(&v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin})
	require.NoError(t, err)

	base := claudeconfig.Production("fixture-model", "original instructions")
	base.Git = gitPolicy
	base.MCPServers = map[string]claudeconfig.MCPServer{"mainloop": {Type: "http", URL: "http://mainloop-mcp." + namespace + ".svc.cluster.local/mcp"}}
	baseJSON, err := json.Marshal(base)
	require.NoError(t, err)

	developmentImage := "fixture.test/d@sha256:" + strings.Repeat("a", 64)
	payloadImage := "fixture.test/r@sha256:" + strings.Repeat("b", 64)
	selection := translator.Composition{
		DevelopmentImage: developmentImage, PayloadImage: payloadImage, CLIVersion: claudeconfig.PinnedClaudeVersion,
		Platform: "linux/amd64", PolicyIdentity: "fixture-v1", Provider: translator.HarnessTypeClaude, Schema: payload.Schema,
	}
	baseRevision := translator.Revision{
		AgentName: "mainloop-main", AgentUID: "agent-uid", Namespace: "kagent",
		NativeProvider: translator.HarnessTypeClaude, ConfigJSON: baseJSON,
		AgentCard: &a2apb.AgentCard{Name: "fixture-agent", Version: "v1", Capabilities: &a2apb.AgentCapabilities{},
			SupportedInterfaces: []*a2apb.AgentInterface{{Url: "http://localhost:80", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"}},
			DefaultInputModes:   []string{"text"}, DefaultOutputModes: []string{"text"}},
		WorkerPoolName: "default",
	}
	composed, err := translator.ComposeRevision(baseRevision, selection)
	require.NoError(t, err)
	revisionID, err := composed.Digest()
	require.NoError(t, err)
	template, err := substrate.ActorTemplateForRevision(&composed, revisionID)
	require.NoError(t, err)
	template.Metadata.Uid = "prepared-template-uid"

	environment := &api.DevelopmentEnvironment{Image: developmentImage, Platform: selection.Platform, PolicyIdentity: selection.PolicyIdentity}
	composition := &api.RuntimeComposition{PayloadImage: payloadImage, Provider: string(translator.HarnessTypeClaude), Schema: payload.Schema, CliVersion: selection.CLIVersion}

	sourceSnapshot, err := json.Marshal(database.EnvironmentRevisionSnapshot{BaseRevision: "base", Environment: environment, Composition: composition})
	require.NoError(t, err)

	revision := &database.RuntimeRevision{
		Revision: "prepared", Namespace: "kagent", AgentName: "mainloop-main", AgentUID: "agent-uid",
		SourceSnapshot:        sourceSnapshot,
		ActorTemplateAtespace: template.GetMetadata().GetAtespace(), ActorTemplateName: template.GetMetadata().GetName(), ActorTemplateUID: template.GetMetadata().GetUid(),
	}

	session := &api.Session{
		Id: "session-id", Creator: auth.MainloopService, DevelopmentEnvironment: environment, RuntimeComposition: composition,
		Workspace: &api.Workspace{Repo: "https://github.com/owner/repo"},
		Credentials: []*api.SessionCredential{
			{Origin: readOrigin, Header: "authorization", SecretRef: &api.SecretKeyReference{Name: namespace + "-git-read-x", Key: "authorization"}},
			{Origin: pushOrigin, Header: "authorization", SecretRef: &api.SecretKeyReference{Name: namespace + "-git-push-x", Key: "authorization"}},
		},
	}

	actor := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Name: template.GetMetadata().GetName(), Atespace: template.GetMetadata().GetAtespace(), Uid: "actor-uid"},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: template.GetMetadata().GetAtespace(), Name: template.GetMetadata().GetName()},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	return session, revision, actor, template
}

// A Harness built the same way for the "mainloop-dev" Git instance as for the
// default "mainloop" instance must be accepted identically.
func TestCheckPreparationRuntimeAcceptsSecondGitInstance(t *testing.T) {
	for _, namespace := range []string{"mainloop", "mainloop-dev"} {
		t.Run(namespace, func(t *testing.T) {
			session, revision, actor, template := checkPreparationRuntimeFixture(t, namespace)
			_, err := CheckPreparationRuntime(session, revision, actor, template)
			require.NoError(t, err)
		})
	}
}
