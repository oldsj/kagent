package auth

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
)

func TestServiceTokenAuthentication(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	current, next := filepath.Join(t.TempDir(), "current"), filepath.Join(t.TempDir(), "next")
	old, newToken := strings.Repeat("a", 32), strings.Repeat("b", 32)
	require.NoError(t, os.WriteFile(current, []byte(old+"\n"), 0600))
	require.NoError(t, os.WriteFile(next, []byte(newToken), 0600))
	provider, err := NewServiceTokenAuthenticator(current, next, policy)
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		values  []string
		allowed bool
	}{
		{"missing", nil, false}, {"bad", []string{"Bearer " + strings.Repeat("c", 32)}, false},
		{"current", []string{"Bearer " + old}, true}, {"next", []string{"Bearer " + newToken}, true},
		{"duplicate", []string{"Bearer " + old, "Bearer " + old}, false}, {"comma", []string{"Bearer " + old + ",Bearer " + newToken}, false},
		{"extra space", []string{"Bearer  " + old}, false}, {"basic", []string{"Basic " + old}, false}, {"padding only", []string{"Bearer " + strings.Repeat("=", 32)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Authorization": tc.values, "X-User-Id": {"mallory"}, "X-Agent-Name": {"kagent/victim"}}
			session, err := provider.Authenticate(t.Context(), headers, url.Values{"user_id": {"mallory"}})
			if !tc.allowed {
				require.Error(t, err)
				require.Nil(t, session)
				return
			}
			require.NoError(t, err)
			require.Equal(t, auth.Principal{User: auth.User{ID: "mainloop"}, Service: "mainloop"}, session.Principal())
			request, _ := http.NewRequest(http.MethodPost, "http://runtime", nil)
			request.Header.Set("Authorization", "Bearer "+old)
			require.NoError(t, provider.UpstreamAuth(request, session, auth.Principal{}))
			require.Empty(t, request.Header.Get("Authorization"))
		})
	}
	require.NoError(t, os.WriteFile(current, []byte(newToken), 0600))
	_, err = provider.Authenticate(t.Context(), http.Header{"Authorization": {"Bearer " + old}}, nil)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(next, []byte("invalid"), 0600))
	_, err = provider.Authenticate(t.Context(), http.Header{"Authorization": {"Bearer " + newToken}}, nil)
	require.Error(t, err)
	_, err = NewServiceTokenAuthenticator(current, next, policy)
	require.Error(t, err)
	_, err = NewServiceTokenAuthenticator("", "", policy)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(current, []byte(strings.Repeat("x", maxServiceTokenBytes+1)), 0600))
	_, err = NewServiceTokenAuthenticator(current, "", policy)
	require.Error(t, err)
}
