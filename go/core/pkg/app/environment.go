package app

import (
	"strings"

	"github.com/kagent-dev/kagent/go/core/internal/controller"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

// Only the verified service-token mode installs a selection policy/preparer.
// Insecure and trusted-proxy Session creation retain the default-deny boundary.
func environmentOptions(authenticator auth.AuthProvider, catalog controller.RuntimePayloadCatalog, preparer sessionsvc.EnvironmentPreparer) []sessionsvc.Option {
	service, ok := authenticator.(*authimpl.ServiceTokenAuthenticator)
	if !ok {
		return nil
	}
	policy := service.Authorizer().(*controlauth.Policy)
	platforms := make([]string, 0, len(catalog))
	for key := range catalog {
		_, platform, _ := strings.Cut(key, "/")
		platforms = append(platforms, platform)
	}
	return []sessionsvc.Option{sessionsvc.WithEnvironmentPolicy(policy.WithRuntimePlatforms(platforms)), sessionsvc.WithEnvironmentPreparer(preparer)}
}
