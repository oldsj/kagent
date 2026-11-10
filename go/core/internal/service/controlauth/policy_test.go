package controlauth_test

import (
	"context"
	"strings"
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

const devImage = "/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestCheckDevelopmentEnvironment(t *testing.T) {
	newPolicy := func(t *testing.T, registries ...string) *controlauth.Policy {
		t.Helper()
		config := validConfig()
		config.DevelopmentEnvironmentRegistries = registries
		policy, err := controlauth.New(config)
		require.NoError(t, err)
		return policy
	}
	agent := &apiv1.ResourceReference{Namespace: "kagent", Name: "claude"}
	environment := func(image string) *apiv1.DevelopmentEnvironment {
		return &apiv1.DevelopmentEnvironment{Image: image, Platform: "linux/arm64", PolicyIdentity: "project/v1"}
	}

	t.Run("Given an allowlisted registry, when an allowlisted Agent selects an image from it, assert that the selection is admitted", func(t *testing.T) {
		policy := newPolicy(t, "registry.example:5000")

		err := policy.CheckDevelopmentEnvironment(caller(), agent, environment("registry.example:5000"+devImage))

		require.NoError(t, err)
	})

	scenarios := []struct {
		name       string
		registries []string
		agent      *apiv1.ResourceReference
		image      string
	}{
		{"no allowlisted registries", nil, agent, "registry.example" + devImage},
		{"an image from another registry", []string{"registry.example"}, agent, "other.example" + devImage},
		{"an image on the allowlisted host with another port", []string{"registry.example"}, agent, "registry.example:5000" + devImage},
		{"an image without a registry authority", []string{"registry.example"}, agent, "dev@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
		{"an Agent outside the allowlist", []string{"registry.example"}, &apiv1.ResourceReference{Namespace: "kagent", Name: "other"}, "registry.example" + devImage},
	}
	for _, scenario := range scenarios {
		t.Run("Given "+scenario.name+", when a development image is selected, assert that the selection is denied", func(t *testing.T) {
			policy := newPolicy(t, scenario.registries...)

			err := policy.CheckDevelopmentEnvironment(caller(), scenario.agent, environment(scenario.image))

			require.ErrorIs(t, err, controlauth.ErrDenied)
		})
	}

	t.Run("Given no allowlisted registries, when internal controller work selects an image, assert that it is not exempt", func(t *testing.T) {
		policy := newPolicy(t)
		internal := auth.AuthSessionTo(context.Background(), auth.ControlPlaneSession{})

		err := policy.CheckDevelopmentEnvironment(internal, agent, environment("registry.example"+devImage))

		require.ErrorIs(t, err, controlauth.ErrDenied)
	})

	for _, registry := range []string{"https://registry.example", "registry.example/team", "user@registry.example", "*.example", ""} {
		t.Run("Given a registry entry "+strings.ReplaceAll(registry, "/", "∕")+", when the policy is loaded, assert that it is rejected", func(t *testing.T) {
			config := validConfig()
			config.DevelopmentEnvironmentRegistries = []string{registry}

			_, err := controlauth.New(config)

			require.Error(t, err)
		})
	}
}

func gitInstanceRules(namespace string) []*configv1.SessionCredentialRule {
	read, push := workspace.GitInstanceOrigins(namespace)
	return []*configv1.SessionCredentialRule{
		{SecretNamePattern: namespace + "-git-read-*", Key: "authorization", Origin: read, Header: "authorization", Purpose: "git-read"},
		{SecretNamePattern: namespace + "-git-push-*", Key: "authorization", Origin: push, Header: "authorization", Purpose: "git-push"},
	}
}

func TestNewAcceptsOnlyKnownGitInstanceTuples(t *testing.T) {
	devRead, devPush := workspace.GitInstanceOrigins("mainloop-dev")
	for _, test := range []struct {
		name    string
		rules   []*configv1.SessionCredentialRule
		wantErr bool
	}{
		{"mainloop instance", gitInstanceRules("mainloop"), false},
		{"mainloop-dev instance", gitInstanceRules("mainloop-dev"), false},
		{"both instances", append(gitInstanceRules("mainloop"), gitInstanceRules("mainloop-dev")...), false},
		{"unknown instance", gitInstanceRules("other"), true},
		{"dev origin with the mainloop Secret pattern", []*configv1.SessionCredentialRule{
			{SecretNamePattern: "mainloop-git-read-*", Key: "authorization", Origin: devRead, Header: "authorization", Purpose: "git-read"},
		}, true},
		{"mainloop origin with the dev Secret pattern", []*configv1.SessionCredentialRule{
			{SecretNamePattern: "mainloop-dev-git-push-*", Key: "authorization", Origin: workspace.PushProxyOrigin, Header: "authorization", Purpose: "git-push"},
		}, true},
		{"dev read rule on the dev push proxy", []*configv1.SessionCredentialRule{
			{SecretNamePattern: "mainloop-dev-git-read-*", Key: "authorization", Origin: devPush, Header: "authorization", Purpose: "git-read"},
		}, true},
		{"mcp rule on the dev Git proxy", []*configv1.SessionCredentialRule{
			{SecretNamePattern: "mainloop-mcp-*", Key: "authorization", Origin: devRead, Header: "authorization", Purpose: "mcp"},
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := controlauth.New(&configv1.SessionPolicy{Namespace: "kagent", Agents: []string{"claude"}, Credentials: test.rules})
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
	// Any instance's proxy host is reserved, approved or not.
	otherRead, _ := workspace.GitInstanceOrigins("other")
	_, err := controlauth.New(&configv1.SessionPolicy{Namespace: "kagent", Agents: []string{"claude"}, CleartextMcpOrigins: []string{otherRead}})
	require.Error(t, err)
}

func TestCheckCreateSessionMatchesGitCredentialsToTheirInstance(t *testing.T) {
	policy, err := controlauth.New(&configv1.SessionPolicy{Namespace: "kagent", Agents: []string{"claude"}, Credentials: append(gitInstanceRules("mainloop"), gitInstanceRules("mainloop-dev")...)})
	require.NoError(t, err)
	devRead, _ := workspace.GitInstanceOrigins("mainloop-dev")
	check := func(credentials ...*apiv1.SessionCredential) error {
		return policy.CheckCreateSession(caller(), &apiv1.ResourceReference{Namespace: "kagent", Name: "claude"}, credentials)
	}
	require.NoError(t, check(credential(devRead, "authorization", "mainloop-dev-git-read-abc", "authorization")))
	require.Error(t, check(credential(devRead, "authorization", "mainloop-git-read-abc", "authorization")))
	require.Error(t, check(credential(workspace.ReadProxyOrigin, "authorization", "mainloop-dev-git-read-abc", "authorization")))
}
