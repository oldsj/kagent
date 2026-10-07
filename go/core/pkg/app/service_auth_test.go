package app

import (
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceAuthenticatorDefaultsToScopedPolicy(t *testing.T) {
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"mainloop-main"}})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "current")
	require.NoError(t, os.WriteFile(file, []byte(strings.Repeat("a", 32)), 0600))
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	gotProvider, gotPolicy := (Options{Authenticator: provider}).resolve()
	require.Same(t, provider, gotProvider)
	require.Same(t, policy, gotPolicy)
	require.Error(t, gotPolicy.Check(t.Context(), auth.Principal{Service: auth.MainloopService, User: auth.User{ID: auth.MainloopService}}, auth.VerbCreate, auth.Resource{Type: "Agent", Namespace: "kagent", Name: "mainloop-main"}))
}
