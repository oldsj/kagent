package egress_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kagent-dev/kagent/go/core/internal/egress"
)

func TestFilterGitRestricted(t *testing.T) {
	t.Run("Given grants that reach GitHub and the Git proxies, when they are filtered, assert that only unrelated access remains", func(t *testing.T) {
		grants := egress.Access{
			Destinations: []string{
				"https://pypi.org:443",
				"https://github.com:443",
				"https://api.github.com:443",
				"https://*.githubusercontent.com:443",
				"https://*.github.com:443",
				"http://mainloop-git-read.mainloop.svc.cluster.local:80",
				"http://mainloop-git-push.mainloop.svc.cluster.local:8080",
				"https://notgithub.com:443",
			},
			Credentials: []egress.Credential{
				{Origin: "https://pypi.org:443", Header: "authorization", URI: "ate-secret://k8s.io/default/team/pypi/token"},
				{Origin: "https://api.github.com:443", Header: "authorization", URI: "ate-secret://k8s.io/default/team/other/token"},
				{Origin: "https://example.com:443", Header: "authorization", URI: "ate-secret://k8s.io/default/team/mainloop-git-auth/authorization"},
			},
		}
		original := egress.Access{Destinations: append([]string(nil), grants.Destinations...), Credentials: append([]egress.Credential(nil), grants.Credentials...)}

		kept, dropped := egress.FilterGitRestricted(grants)

		require.Equal(t, []string{"https://pypi.org:443", "https://notgithub.com:443"}, kept.Destinations)
		require.Equal(t, []egress.Credential{{Origin: "https://pypi.org:443", Header: "authorization", URI: "ate-secret://k8s.io/default/team/pypi/token"}}, kept.Credentials)
		require.Len(t, dropped, 8, "six destinations and two credentials")
		require.Equal(t, original, grants, "inputs are not modified")
	})

	t.Run("Given wildcard grants over any Git instance's proxy listeners, when they are filtered, assert that they are dropped", func(t *testing.T) {
		kept, dropped := egress.FilterGitRestricted(egress.Access{Destinations: []string{
			"http://*.mainloop.svc.cluster.local:80",
			"http://*.mainloop-dev.svc.cluster.local:80",
			"http://*.svc.cluster.local:80",
			"http://mainloop-git-read.mainloop-dev.svc.cluster.local:80",
			"http://*.mainloop-mcp.mainloop.svc.cluster.local:80",
			"https://*.example.com:443",
		}})
		require.Equal(t, []string{"http://*.mainloop-mcp.mainloop.svc.cluster.local:80", "https://*.example.com:443"}, kept.Destinations)
		require.Len(t, dropped, 4)
	})

	t.Run("Given a grant origin that cannot be parsed, when it is filtered, assert that it is dropped rather than passed through", func(t *testing.T) {
		kept, dropped := egress.FilterGitRestricted(egress.Access{Destinations: []string{"://bad", "https://ok.example.com:443"}})
		require.Equal(t, []string{"https://ok.example.com:443"}, kept.Destinations)
		require.Equal(t, []string{"://bad"}, dropped)
	})

	t.Run("Given grants with no Git access, when they are filtered, assert that nothing is dropped", func(t *testing.T) {
		grants := egress.Access{Destinations: []string{"https://pypi.org:443"}}
		kept, dropped := egress.FilterGitRestricted(grants)
		require.Equal(t, grants.Destinations, kept.Destinations)
		require.Empty(t, dropped)
	})
}
