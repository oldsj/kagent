package translator_test

import (
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/stretchr/testify/require"
)

func TestCompileGitProxy(t *testing.T) {
	spec := &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	policy, destinations, err := v2translator.CompileGit(spec)
	require.NoError(t, err)
	copy := spec.DeepCopy()
	*copy.ReadProxyOrigin = "changed"
	*copy.PushProxyOrigin = "changed"
	copy.Origins[0] = "changed"
	require.Equal(t, workspace.ReadProxyOrigin, *spec.ReadProxyOrigin)
	require.Equal(t, workspace.PushProxyOrigin, *spec.PushProxyOrigin)
	require.Equal(t, "github.com", spec.Origins[0])
	require.Equal(t, []string{"github.com"}, v2translator.GitOrigins(policy))
	require.Equal(t, []string{workspace.ReadProxyOrigin + ":80", workspace.PushProxyOrigin + ":80"}, destinations)
	*spec.ReadProxyOrigin = "bad"
	require.Equal(t, workspace.ReadProxyOrigin, *policy.ReadProxyOrigin)
	_, _, err = v2translator.CompileGit(spec)
	require.Error(t, err)
	spec = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, CredentialSecretRef: &v1alpha3.SecretKeyReference{Name: "legacy", Key: "authorization"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin)}
	_, _, err = v2translator.CompileGit(spec)
	require.Error(t, err)
	policy, destinations, err = v2translator.CompileGit(&v1alpha3.HarnessGit{Origins: []string{"github.com"}})
	require.NoError(t, err)
	require.Equal(t, []string{"https://github.com:443"}, destinations)
	require.NoError(t, v2translator.ValidateGitEgress(policy, destinations, nil), "explicit standalone legacy retained")
	policy, destinations, err = v2translator.CompileGit(nil)
	require.NoError(t, err)
	require.Nil(t, policy)
	require.Nil(t, destinations)
}
func TestEffectiveGitEgress(t *testing.T) {
	for _, origin := range []string{"https://*.example.com:443", "https://127.0.0.1:443", "http://[::1]:80", "tcp://github.com:22"} {
		_, err := substrate.ActorEgressPolicy("mainloop", []string{origin}, nil)
		require.Error(t, err, origin)
	}
	policy := &workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	require.NoError(t, v2translator.ValidateGitEgress(policy, []string{workspace.ReadProxyOrigin + ":80", workspace.PushProxyOrigin + ":80", "https://pypi.org:443"}, nil))
	for _, host := range []string{"github.com", "api.github.com", "uploads.github.com", "raw.github.com", "gist.github.com", "codeload.github.com", "ssh.github.com", "graphql.github.com", "raw.githubusercontent.com", "githubusercontent.com", "objects.githubusercontent.com", "githubassets.com", "assets.githubassets.com", "github.io", "owner.github.io", "API.GITHUB.COM."} {
		for _, origin := range []string{"https://" + host + ":443", "http://" + host + ":80", "https://" + host + ":8443"} {
			require.Error(t, v2translator.ValidateGitEgress(policy, []string{origin}, nil), origin)
			require.Error(t, v2translator.ValidateGitEgress(nil, []string{origin}, nil), origin)
		}
	}
	for _, origin := range []string{workspace.ReadProxyOrigin, workspace.PushProxyOrigin, "https://mainloop-git-read.mainloop.svc.cluster.local:443", workspace.ReadProxyOrigin + ":8004"} {
		require.Error(t, v2translator.ValidateGitEgress(nil, []string{origin}, nil))
	}
	require.Error(t, v2translator.ValidateGitEgress(&workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin)}, []string{workspace.PushProxyOrigin + ":80"}, nil))
	for _, binding := range []egress.Credential{
		{Hostname: "api.openai.com", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/mainloop-git-auth/authorization"},
		{Hostname: "mainloop-git-read.mainloop.svc.cluster.local", Header: "x-extra", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
		{Hostname: "MAINLOOP-git-push.mainloop.svc.cluster.local.", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
		{Hostname: "raw.githubusercontent.com", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
	} {
		require.Error(t, v2translator.ValidateGitEgress(policy, nil, []egress.Credential{binding}))
		require.Error(t, v2translator.ValidateGitEgress(nil, nil, []egress.Credential{binding}))
	}
}
