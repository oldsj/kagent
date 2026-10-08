package substrate

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGenerationNamesAndURIsNeverReuse(t *testing.T) {
	session := uuid.NewString()
	names, uris := map[string]bool{}, map[string]bool{}
	for range 100 {
		g, token, err := NewRuntimeGeneration(session, "team-a", "kagent")
		require.NoError(t, err)
		require.Len(t, g.ActorName, 61)
		require.False(t, names[g.ActorName])
		require.False(t, uris[g.CredentialURI])
		names[g.ActorName], uris[g.CredentialURI] = true, true
		digest := sha256.Sum256([]byte(token))
		require.Equal(t, digest[:], g.TokenDigest)
		require.NotContains(t, g.CredentialURI, token)
	}
}

type lostSecretReply struct {
	client.Client
	lost bool
}

func (c *lostSecretReply) Create(ctx context.Context, obj client.Object, options ...client.CreateOption) error {
	if err := c.Client.Create(ctx, obj, options...); err != nil {
		return err
	}
	if !c.lost {
		c.lost = true
		return context.DeadlineExceeded
	}
	return nil
}
func TestSecretIssuanceReconcilesExactOriginalDigest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	kube := &lostSecretReply{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	issuer := NewRuntimeCredentialIssuer(kube, "kagent")
	g, token, err := NewRuntimeGeneration(uuid.NewString(), "team-a", "kagent")
	require.NoError(t, err)
	require.Error(t, issuer.Ensure(t.Context(), g, token), "lost reply is not proof of failure")
	require.NoError(t, issuer.Ensure(t.Context(), g, ""), "exact immutable issuance can be reconciled")
	key, err := issuer.key(g)
	require.NoError(t, err)
	secret := &corev1.Secret{}
	require.NoError(t, kube.Get(t.Context(), key, secret))
	require.True(t, *secret.Immutable)
	wrong := g
	wrong.TokenDigest = make([]byte, 32)
	require.Error(t, issuer.Ensure(t.Context(), wrong, ""), "never adopt a foreign Secret")
	require.NoError(t, issuer.Delete(t.Context(), g))
	require.True(t, apierrors.IsNotFound(kube.Get(t.Context(), key, secret)))
	require.Error(t, issuer.Ensure(t.Context(), g, ""), "missing original material must hold rather than rotate")
}

func TestRuntimePolicyCannotReflectCapability(t *testing.T) {
	g, token, err := NewRuntimeGeneration(uuid.NewString(), "team-a", "kagent")
	require.NoError(t, err)
	callback := "http://kagent-controller.kagent:8083"
	for _, test := range []struct {
		name         string
		destinations []string
		credentials  []egress.Credential
	}{
		{"alternate port", []string{"http://kagent-controller.kagent:8084"}, nil},
		{"alternate scheme", []string{"https://kagent-controller.kagent:8083"}, nil},
		{"echo header", []string{"https://echo.example:443"}, []egress.Credential{{Hostname: "echo.example", Header: egress.RuntimeTokenHeader, URI: g.CredentialURI}}},
		{"callback credential", nil, []egress.Credential{{Hostname: "kagent-controller.kagent", Header: "authorization", URI: "ate-secret://k8s.io/default/team-a/auth/token"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := RuntimeEgressPolicy(g, callback, test.destinations, test.credentials)
			require.Error(t, err)
		})
	}
	policy, err := RuntimeEgressPolicy(g, callback, []string{callback, "https://proxy.golang.org:443"}, nil)
	require.NoError(t, err)
	require.NotContains(t, policy.String(), token)
	for _, rule := range policy.Rules {
		if http := rule.GetHttp(); http != nil {
			require.Equal(t, []string{"kagent-controller.kagent"}, http.Hostnames)
			require.Equal(t, []int32{8083}, http.Ports.Numbers)
			require.Equal(t, g.CredentialURI, http.Effects.ReplaceHeaders[0].CredentialUri)
		}
		if https := rule.GetHttps(); https != nil {
			require.Nil(t, https.Effects)
		}
	}
	require.NotContains(t, strings.Join([]string{g.ActorName, g.CredentialURI}, " "), token)
}
