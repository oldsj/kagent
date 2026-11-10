package controlauth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/workspace"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

const mcpOrigin = "http://mainloop-mcp.mainloop.svc.cluster.local"

func validConfig() *configv1.SessionPolicy {
	return &configv1.SessionPolicy{
		Namespace: "kagent", Agents: []string{"claude", "codex"},
		CleartextMcpOrigins: []string{mcpOrigin},
		Credentials: []*configv1.SessionCredentialRule{
			{SecretNamePattern: "mainloop-git-read-*", Key: "authorization", Origin: workspace.ReadProxyOrigin, Header: "authorization", Purpose: "git-read"},
			{SecretNamePattern: "mainloop-git-push-*", Key: "authorization", Origin: workspace.PushProxyOrigin, Header: "authorization", Purpose: "git-push"},
			{SecretNamePattern: "mainloop-mcp-*", Key: "authorization", Origin: mcpOrigin, Header: "authorization", Purpose: "mcp"},
		},
	}
}

func credential(origin, header, secret, key string) *apiv1.SessionCredential {
	return &apiv1.SessionCredential{Origin: origin, Header: header, SecretRef: &apiv1.SecretKeyReference{Name: secret, Key: key}}
}

func caller() context.Context {
	return auth.AuthSessionTo(context.Background(), &authimpl.SimpleSession{P: auth.Principal{ID: "kubernetes:cluster/sa-uid"}})
}

func TestNewRejectsRulesThatWidenGitAccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*configv1.SessionPolicy)
	}{
		{"no namespace", func(c *configv1.SessionPolicy) { c.Namespace = "" }},
		{"no agents", func(c *configv1.SessionPolicy) { c.Agents = nil }},
		{"git-read with a wider Secret pattern", func(c *configv1.SessionPolicy) { c.Credentials[0].SecretNamePattern = "mainloop-*" }},
		{"git-read on the push proxy", func(c *configv1.SessionPolicy) { c.Credentials[0].Origin = workspace.PushProxyOrigin }},
		{"git-push with another key", func(c *configv1.SessionPolicy) { c.Credentials[1].Key = "token" }},
		{"mcp rule on a Git proxy", func(c *configv1.SessionPolicy) { c.Credentials[2].Origin = workspace.ReadProxyOrigin }},
		{"mcp over unlisted cleartext", func(c *configv1.SessionPolicy) { c.CleartextMcpOrigins = nil }},
		{"cleartext origin outside the cluster", func(c *configv1.SessionPolicy) { c.CleartextMcpOrigins = []string{"http://example.com"} }},
		{"cleartext origin that is a Git proxy", func(c *configv1.SessionPolicy) { c.CleartextMcpOrigins = []string{workspace.ReadProxyOrigin} }},
		{"rule origin with a path", func(c *configv1.SessionPolicy) { c.Credentials[2].Origin = mcpOrigin + "/mcp" }},
		{"header other than authorization", func(c *configv1.SessionPolicy) { c.Credentials[2].Header = "x-api-key" }},
		{"unknown purpose", func(c *configv1.SessionPolicy) { c.Credentials[2].Purpose = "other" }},
		{"malformed Secret pattern", func(c *configv1.SessionPolicy) { c.Credentials[2].SecretNamePattern = "[" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(config)
			_, err := controlauth.New(config)
			require.Error(t, err)
		})
	}
	_, err := controlauth.New(validConfig())
	require.NoError(t, err)
}

func TestCheckCreateSession(t *testing.T) {
	policy, err := controlauth.New(validConfig())
	require.NoError(t, err)
	agent := &apiv1.ResourceReference{Namespace: "kagent", Name: "claude"}
	read := credential(workspace.ReadProxyOrigin, "Authorization", "mainloop-git-read-abc", "authorization")
	push := credential(workspace.PushProxyOrigin, "authorization", "mainloop-git-push-abc", "authorization")
	mcp := credential(mcpOrigin, "authorization", "mainloop-mcp-binding", "authorization")

	for _, test := range []struct {
		name        string
		agent       *apiv1.ResourceReference
		credentials []*apiv1.SessionCredential
		allowed     bool
	}{
		{"an allowed Agent without credentials", agent, nil, true},
		{"every approved tuple at once", agent, []*apiv1.SessionCredential{read, push, mcp}, true},
		{"an Agent that is not listed", &apiv1.ResourceReference{Namespace: "kagent", Name: "other"}, nil, false},
		{"a listed Agent name in another namespace", &apiv1.ResourceReference{Namespace: "team", Name: "claude"}, nil, false},
		{"a Secret outside the pattern", agent, []*apiv1.SessionCredential{credential(workspace.ReadProxyOrigin, "authorization", "model-api-key", "authorization")}, false},
		{"a push Secret offered to the read proxy", agent, []*apiv1.SessionCredential{credential(workspace.ReadProxyOrigin, "authorization", "mainloop-git-push-abc", "authorization")}, false},
		{"another key of an approved Secret", agent, []*apiv1.SessionCredential{credential(mcpOrigin, "authorization", "mainloop-mcp-binding", "other")}, false},
		{"an approved Secret on another origin", agent, []*apiv1.SessionCredential{credential("https://example.com", "authorization", "mainloop-mcp-binding", "authorization")}, false},
		{"an approved Secret under another header", agent, []*apiv1.SessionCredential{credential(mcpOrigin, "x-api-key", "mainloop-mcp-binding", "authorization")}, false},
		{"two credentials for one origin and header", agent, []*apiv1.SessionCredential{read, credential(workspace.ReadProxyOrigin, "AUTHORIZATION", "mainloop-git-read-def", "authorization")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := policy.CheckCreateSession(caller(), test.agent, test.credentials)
			if test.allowed {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, controlauth.ErrDenied)
		})
	}

	t.Run("Given internal controller work, when it creates a Session for an unlisted Agent, assert that the policy does not apply", func(t *testing.T) {
		internal := auth.AuthSessionTo(context.Background(), auth.ControlPlaneSession{})
		require.NoError(t, policy.CheckCreateSession(internal, &apiv1.ResourceReference{Namespace: "team", Name: "scheduled"}, nil))
	})

	t.Run("Given a runtime Actor, when it creates a Session for an unlisted Agent, assert that it is refused like any API caller", func(t *testing.T) {
		actor := auth.AuthSessionTo(context.Background(), &authimpl.SimpleSession{P: auth.Principal{ID: "owner", RuntimeBinding: &auth.RuntimeBinding{SessionID: "s"}}})
		require.ErrorIs(t, policy.CheckCreateSession(actor, &apiv1.ResourceReference{Namespace: "kagent", Name: "other"}, nil), controlauth.ErrDenied)
	})

	t.Run("Given a policy with no credential rules, when a Session carries a credential, assert that it is refused", func(t *testing.T) {
		config := validConfig()
		config.Credentials, config.CleartextMcpOrigins = nil, nil
		bare, err := controlauth.New(config)
		require.NoError(t, err)
		require.ErrorIs(t, bare.CheckCreateSession(caller(), agent, []*apiv1.SessionCredential{mcp}), controlauth.ErrDenied)
		require.NoError(t, bare.CheckCreateSession(caller(), agent, nil))
	})
}
