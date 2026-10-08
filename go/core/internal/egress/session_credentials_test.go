package egress_test

import (
	"fmt"
	"testing"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func credential(origin, header, key string) *apiv1alpha1.SessionCredential {
	return &apiv1alpha1.SessionCredential{Origin: origin, Header: header, SecretRef: &apiv1alpha1.SecretKeyReference{Name: "tokens", Key: key}}
}

func TestSessionCredentials(t *testing.T) {
	destinations := []string{"http://mcp.example.com:80", "https://api.example.com:443"}
	revision := []egress.Credential{{Hostname: "api.example.com", Header: "authorization", Prefix: "Bearer ", URI: "ate-secret://k8s.io/default/team-a/model/key"}}
	for _, tc := range []struct {
		name    string
		input   []*apiv1alpha1.SessionCredential
		wantErr bool
	}{
		{"HTTP full header", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com", "Authorization", "one")}, false},
		{"unlisted origin", []*apiv1alpha1.SessionCredential{credential("https://other.example.com", "authorization", "one")}, true},
		{"wrong scheme", []*apiv1alpha1.SessionCredential{credential("https://mcp.example.com", "authorization", "one")}, true},
		{"wrong port", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com:81", "authorization", "one")}, true},
		{"path", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com/mcp", "authorization", "one")}, true},
		{"revision clash", []*apiv1alpha1.SessionCredential{credential("https://API.EXAMPLE.COM", "Authorization", "one")}, true},
		{"duplicate", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com", "Authorization", "one"), credential("http://mcp.example.com:80", "authorization", "one")}, true},
		{"malformed header", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com", "bad header", "one")}, true},
		{"missing Secret", []*apiv1alpha1.SessionCredential{{Origin: "http://mcp.example.com", Header: "authorization"}}, true},
		{"invalid key", []*apiv1alpha1.SessionCredential{credential("http://mcp.example.com", "authorization", "../one")}, true},
		{"nil", []*apiv1alpha1.SessionCredential{nil}, true},
		{"too many", make([]*apiv1alpha1.SessionCredential, 5), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := egress.SessionCredentials("team-a", tc.input, destinations, revision)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 2)
			require.Equal(t, "authorization", got[1].Header)
			require.Empty(t, got[1].Prefix, "Secret contains the complete header value")
			require.Equal(t, "ate-secret://k8s.io/default/team-a/tokens/one", got[1].URI)
		})
	}
}

// Golden wire fixture shared with Mainloop's hand-written protobuf encoder.
func TestSessionCredentialWireContract(t *testing.T) {
	input := &apiv1alpha1.CreateSessionRequest{Credentials: []*apiv1alpha1.SessionCredential{{Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-agent-tokens", Key: "binding-id"}}}}
	data, err := proto.Marshal(input)
	require.NoError(t, err)
	require.Equal(t, sessionCredentialWireHex, fmt.Sprintf("%x", data))
	decoded := &apiv1alpha1.CreateSessionRequest{}
	require.NoError(t, proto.Unmarshal(data, decoded))
	require.True(t, proto.Equal(input, decoded))
}

const sessionCredentialWireHex = "3a640a2e687474703a2f2f6d61696e6c6f6f702d6d63702e6d61696e6c6f6f702e7376632e636c75737465722e6c6f63616c120d417574686f72697a6174696f6e1a230a156d61696e6c6f6f702d6167656e742d746f6b656e73120a62696e64696e672d6964"

func TestGitReferencesRemainLazyAndSeparate(t *testing.T) {
	destinations := []string{"http://mainloop-mcp.mainloop.svc.cluster.local:80", "http://mainloop-git-read.mainloop.svc.cluster.local:80", "http://mainloop-git-push.mainloop.svc.cluster.local:80", "https://api.openai.com:443"}
	refs := []*apiv1alpha1.SessionCredential{
		{Origin: "http://mainloop-mcp.mainloop.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-mcp-binding-missing", Key: "authorization"}},
		{Origin: "http://mainloop-git-read.mainloop.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-read-missing", Key: "authorization"}},
		{Origin: "http://mainloop-git-push.mainloop.svc.cluster.local", Header: "Authorization", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "mainloop-git-push-missing", Key: "authorization"}},
	}
	provider := egress.Credential{Hostname: "api.openai.com", Header: "authorization", Prefix: "Bearer ", URI: "ate-secret://k8s.io/default/kagent/model/key"}
	got, err := egress.SessionCredentials("kagent", refs, destinations, []egress.Credential{provider})
	require.NoError(t, err)
	require.Len(t, got, 4)
	for _, binding := range got {
		if binding.Hostname != "api.openai.com" {
			require.Empty(t, binding.Prefix)
			require.Contains(t, binding.URI, "/authorization")
		}
	}
	refs = append(refs, &apiv1alpha1.SessionCredential{Origin: "https://api.openai.com", Header: "x-extra", SecretRef: &apiv1alpha1.SecretKeyReference{Name: "extra", Key: "key"}})
	_, err = egress.SessionCredentials("kagent", refs, destinations, []egress.Credential{provider})
	require.NoError(t, err, "max four retained")
	refs = append(refs, proto.CloneOf(refs[0]))
	_, err = egress.SessionCredentials("kagent", refs, destinations, nil)
	require.Error(t, err)
	refs = refs[:3]
	refs[1].Header = egress.RuntimeTokenHeader
	_, err = egress.SessionCredentials("kagent", refs, destinations, nil)
	require.Error(t, err)
	refs[1].Header = "Authorization"
	refs[2].Origin = refs[1].Origin
	_, err = egress.SessionCredentials("kagent", refs, destinations, nil)
	require.Error(t, err, "host/header read/push collision")
}
