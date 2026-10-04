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
