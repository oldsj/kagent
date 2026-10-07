package controlauth

import (
	"testing"

	api "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
)

func TestServicePolicy(t *testing.T) {
	p, err := New(Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, Credentials: []CredentialRule{{Namespace: "kagent", SecretNamePattern: "mainloop-mcp-binding-*", Key: "authorization", Origin: "https://mainloop.example.com", Header: "authorization", Purpose: "mcp"}}})
	require.NoError(t, err)
	principal := auth.Principal{User: auth.User{ID: "mainloop"}, Service: "mainloop"}
	for _, tc := range []struct {
		kind    string
		verb    auth.Verb
		name    string
		allowed bool
	}{
		{"Agent", auth.VerbGet, "mainloop-main", true}, {"Agent", auth.VerbGet, "other", false}, {"Agent", auth.VerbCreate, "mainloop-main", false},
		{"Session", auth.VerbGet, "id", true}, {"Session", auth.VerbCreate, "id/shares", false}, {"SessionAllCreators", auth.VerbGet, "", false},
		{"Harness", auth.VerbGet, "mainloop-main", false}, {"ModelConfig", auth.VerbGet, "mainloop-main", false},
	} {
		t.Run(tc.kind+"/"+string(tc.verb)+"/"+tc.name, func(t *testing.T) {
			err := p.Check(t.Context(), principal, tc.verb, auth.Resource{Type: tc.kind, Namespace: "kagent", Name: tc.name})
			require.Equal(t, tc.allowed, err == nil)
		})
	}
	require.Error(t, p.Check(t.Context(), auth.Principal{User: auth.User{ID: "mainloop"}}, auth.VerbGet, auth.Resource{Type: "Session"}))
	internalCtx := auth.AuthSessionTo(t.Context(), auth.ControlPlaneSession{})
	require.NoError(t, p.Check(internalCtx, auth.Principal{}, auth.VerbUpdate, auth.Resource{Type: "Harness"}))
	agent := &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}
	for _, tc := range []struct {
		name, key, origin, header string
		allowed                   bool
	}{
		{"mainloop-mcp-binding-one", "authorization", "https://mainloop.example.com", "Authorization", true},
		{"database", "authorization", "https://mainloop.example.com", "authorization", false},
		{"mainloop-mcp-binding-one", "password", "https://mainloop.example.com", "authorization", false},
		{"mainloop-mcp-binding-one", "authorization", "https://other.example.com", "authorization", false},
		{"mainloop-mcp-binding-one", "authorization", "https://mainloop.example.com", "x-extra", false},
	} {
		t.Run(tc.name+tc.key+tc.origin+tc.header, func(t *testing.T) {
			err := p.CheckCreateSession(t.Context(), agent, []*api.SessionCredential{{Origin: tc.origin, Header: tc.header, SecretRef: &api.SecretKeyReference{Name: tc.name, Key: tc.key}}})
			require.Equal(t, tc.allowed, err == nil)
		})
	}
	for _, creator := range []string{"mainloop", "other"} {
		require.Equal(t, creator == "mainloop", p.CheckSession(t.Context(), &api.Session{Creator: creator, Agent: agent}) == nil)
	}
	require.Error(t, p.CheckSession(t.Context(), &api.Session{Creator: "mainloop", Agent: &api.ResourceReference{Namespace: "kagent", Name: "other"}}))
	require.Error(t, CheckMethod("/future.Service/Method"))
}

func TestCleartextMCPOrigins(t *testing.T) {
	const origin = "http://mainloop-mcp.mainloop.svc.cluster.local"
	for _, tc := range []struct {
		name            string
		allowed         []string
		origin, purpose string
		wantErr         bool
	}{
		{"exact", []string{origin}, origin, "mcp", false},
		{"exact port", []string{origin + ":8080"}, origin + ":8080", "mcp", false},
		{"empty default", nil, origin, "mcp", true},
		{"other host", []string{origin}, "http://other.mainloop.svc.cluster.local", "mcp", true},
		{"other port", []string{origin}, origin + ":8080", "mcp", true},
		{"other purpose", []string{origin}, origin, "model", true},
		{"tuple path", []string{origin}, origin + "/mcp", "mcp", true},
		{"HTTPS default", nil, "https://example.com", "mcp", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, CleartextMCPOrigins: tc.allowed, Credentials: []CredentialRule{{Namespace: "kagent", SecretNamePattern: "binding", Key: "authorization", Origin: tc.origin, Header: "authorization", Purpose: tc.purpose}}})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			agent := &api.ResourceReference{Namespace: "kagent", Name: "mainloop-main"}
			credential := &api.SessionCredential{Origin: tc.origin, Header: "Authorization", SecretRef: &api.SecretKeyReference{Name: "binding", Key: "authorization"}}
			require.NoError(t, p.CheckCreateSession(t.Context(), agent, []*api.SessionCredential{credential}))
			for _, denied := range []string{origin + "/mcp", "http://other.mainloop.svc.cluster.local", origin + ":9090"} {
				credential.Origin = denied
				require.Error(t, p.CheckCreateSession(t.Context(), agent, []*api.SessionCredential{credential}))
			}
		})
	}
}

func TestMalformedCleartextMCPOrigins(t *testing.T) {
	const origin = "http://mainloop-mcp.mainloop.svc.cluster.local"
	for _, invalid := range []string{
		"", "https://mainloop-mcp.mainloop.svc.cluster.local", "http://example.com",
		"http://*.svc.cluster.local", "http://svc.cluster.local", "http://MAINLOOP.mainloop.svc.cluster.local",
		origin + "/", origin + "/mcp", origin + "?", origin + "?key=value", origin + "#", origin + "#fragment",
		"http://user@mainloop-mcp.mainloop.svc.cluster.local", origin + ":", origin + ":abc", origin + ":0", origin + ":65536",
		"http://bad_name.mainloop.svc.cluster.local", "http://127.0.0.1", origin + ".", " " + origin,
	} {
		t.Run(invalid, func(t *testing.T) {
			_, err := New(Config{Namespace: "kagent", Agents: []string{"mainloop-main"}, CleartextMCPOrigins: []string{invalid}})
			require.Error(t, err)
		})
	}
}

func TestDuplicateCleartextMCPOrigins(t *testing.T) {
	const origin = "http://mainloop-mcp.mainloop.svc.cluster.local"
	_, err := New(Config{
		Namespace:           "kagent",
		Agents:              []string{"mainloop-main"},
		CleartextMCPOrigins: []string{origin, origin},
	})
	require.Error(t, err)
}
