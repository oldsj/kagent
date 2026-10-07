package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/core/internal/controller"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentOptionsOnlyEnableServiceToken(t *testing.T) {
	catalog := controller.RuntimePayloadCatalog{"claude/linux/arm64": {Image: "registry/runtime@sha256:" + strings.Repeat("b", 64), CLIVersion: "2.1.260"}}
	require.Empty(t, environmentOptions(&authimpl.InsecureAuthenticator{}, catalog, nil))
	require.Empty(t, environmentOptions(authimpl.NewProxyAuthenticator("sub"), catalog, nil))
	policy, err := controlauth.New(controlauth.Config{Namespace: "kagent", Agents: []string{"writer"}, DevelopmentEnvironmentRegistries: []string{"registry"}})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte(strings.Repeat("a", 32)), 0600))
	provider, err := authimpl.NewServiceTokenAuthenticator(file, "", policy)
	require.NoError(t, err)
	require.Len(t, environmentOptions(provider, catalog, nil), 2)
}
