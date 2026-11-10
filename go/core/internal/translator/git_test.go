package translator_test

import (
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/stretchr/testify/require"
)

func TestCompileGitProxy(t *testing.T) {
	spec := &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	policy, destinations, err := translator.CompileGit(spec)
	require.NoError(t, err)
	copy := spec.DeepCopy()
	*copy.ReadProxyOrigin = "changed"
	*copy.PushProxyOrigin = "changed"
	copy.Origins[0] = "changed"
	require.Equal(t, workspace.ReadProxyOrigin, *spec.ReadProxyOrigin)
	require.Equal(t, workspace.PushProxyOrigin, *spec.PushProxyOrigin)
	require.Equal(t, "github.com", spec.Origins[0])
	require.Equal(t, []string{"github.com"}, translator.GitOrigins(policy))
	require.Equal(t, []string{workspace.ReadProxyOrigin + ":80", workspace.PushProxyOrigin + ":80"}, destinations)
	*spec.ReadProxyOrigin = "bad"
	require.Equal(t, workspace.ReadProxyOrigin, *policy.ReadProxyOrigin)
	_, _, err = translator.CompileGit(spec)
	require.Error(t, err)
	spec = &v1alpha3.HarnessGit{Origins: []string{"github.com"}, CredentialSecretRef: &v1alpha3.SecretKeyReference{Name: "legacy", Key: "authorization"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin)}
	_, _, err = translator.CompileGit(spec)
	require.Error(t, err)
	policy, destinations, err = translator.CompileGit(&v1alpha3.HarnessGit{Origins: []string{"github.com"}})
	require.NoError(t, err)
	require.Equal(t, []string{"https://github.com:443"}, destinations)
	require.NoError(t, translator.ValidateGitEgress(policy, destinations, nil), "explicit standalone legacy retained")
	policy, destinations, err = translator.CompileGit(nil)
	require.NoError(t, err)
	require.Nil(t, policy)
	require.Nil(t, destinations)
}

func TestCompileGitProxyDevInstance(t *testing.T) {
	devRead, devPush := workspace.GitInstanceOrigins("mainloop-dev")
	spec := &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: &devRead, PushProxyOrigin: &devPush}
	policy, destinations, err := translator.CompileGit(spec)
	require.NoError(t, err)
	require.Equal(t, []string{devRead + ":80", devPush + ":80"}, destinations)
	require.NoError(t, translator.ValidateGitEgress(policy, destinations, nil))
	// A session must never be issued one instance's read origin paired with
	// another instance's push origin.
	mismatched := &v1alpha3.HarnessGit{Origins: []string{"github.com"}, ReadProxyOrigin: &devRead, PushProxyOrigin: new(workspace.PushProxyOrigin)}
	_, _, err = translator.CompileGit(mismatched)
	require.Error(t, err)
}

func TestValidateGitEgressPairsEachInstanceWithItsOwnProxies(t *testing.T) {
	policy := &workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	devRead, devPush := workspace.GitInstanceOrigins("mainloop-dev")
	devPolicy := &workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: &devRead, PushProxyOrigin: &devPush}
	require.NoError(t, translator.ValidateGitEgress(devPolicy, []string{devRead + ":80", devPush + ":80"}, nil))
	require.Error(t, translator.ValidateGitEgress(policy, []string{devRead + ":80", workspace.PushProxyOrigin + ":80"}, nil))
	require.Error(t, translator.ValidateGitEgress(devPolicy, []string{workspace.ReadProxyOrigin + ":80", devPush + ":80"}, nil))
	require.Error(t, translator.ValidateGitEgress(nil, []string{devRead + ":80"}, nil))
	// Every instance's proxy host is reserved, approved or not.
	otherRead, _ := workspace.GitInstanceOrigins("other")
	require.Error(t, translator.ValidateGitEgress(policy, []string{otherRead + ":80"}, nil))
	require.Error(t, translator.ValidateGitEgress(policy, nil, []egress.Credential{{Origin: devRead, Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/binding/key"}}))
}

func TestEffectiveGitEgress(t *testing.T) {
	// Substrate 1.0 policies accept leftmost-label wildcards; IP literals and
	// non-HTTP schemes are still rejected.
	for _, origin := range []string{"https://127.0.0.1:443", "http://[::1]:80", "tcp://github.com:22"} {
		_, err := substrate.ActorEgressPolicy("mainloop", []string{origin}, nil, "http://kagent-controller.kagent:8083")
		require.Error(t, err, origin)
	}
	policy := &workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin), PushProxyOrigin: new(workspace.PushProxyOrigin)}
	require.NoError(t, translator.ValidateGitEgress(policy, []string{workspace.ReadProxyOrigin + ":80", workspace.PushProxyOrigin + ":80", "https://pypi.org:443"}, nil))
	for _, host := range []string{"github.com", "api.github.com", "uploads.github.com", "raw.github.com", "gist.github.com", "codeload.github.com", "ssh.github.com", "graphql.github.com", "raw.githubusercontent.com", "githubusercontent.com", "objects.githubusercontent.com", "githubassets.com", "assets.githubassets.com", "github.io", "owner.github.io", "API.GITHUB.COM."} {
		for _, origin := range []string{"https://" + host + ":443", "http://" + host + ":80", "https://" + host + ":8443"} {
			require.Error(t, translator.ValidateGitEgress(policy, []string{origin}, nil), origin)
			require.Error(t, translator.ValidateGitEgress(nil, []string{origin}, nil), origin)
		}
	}
	for _, origin := range []string{workspace.ReadProxyOrigin, workspace.PushProxyOrigin, "https://mainloop-git-read.mainloop.svc.cluster.local:443", workspace.ReadProxyOrigin + ":8004"} {
		require.Error(t, translator.ValidateGitEgress(nil, []string{origin}, nil))
	}
	require.Error(t, translator.ValidateGitEgress(&workspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: new(workspace.ReadProxyOrigin)}, []string{workspace.PushProxyOrigin + ":80"}, nil))
	for _, binding := range []egress.Credential{
		{Origin: "https://api.openai.com:443", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/mainloop-git-auth/authorization"},
		{Origin: "http://mainloop-git-read.mainloop.svc.cluster.local:80", Header: "x-extra", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
		{Origin: "http://MAINLOOP-git-push.mainloop.svc.cluster.local.:80", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
		{Origin: "https://raw.githubusercontent.com:443", Header: "authorization", URI: "ate-secret://k8s.io/default/kagent/binding/key"},
	} {
		require.Error(t, translator.ValidateGitEgress(policy, nil, []egress.Credential{binding}))
		require.Error(t, translator.ValidateGitEgress(nil, nil, []egress.Credential{binding}))
	}
}
