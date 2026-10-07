package translator_test

import (
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/core/internal/egress"
	translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/stretchr/testify/require"
)

func TestCompileExtraHTTPSOrigins(t *testing.T) {
	destinations, err := translator.CompileExtraHTTPSOrigins([]string{
		"https://PYPI.org/", "https://registry.npmjs.org:443", "https://pypi.org:443/",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"https://pypi.org:443", "https://registry.npmjs.org:443"}, destinations)
	empty, err := translator.CompileExtraHTTPSOrigins(nil)
	require.NoError(t, err)
	require.Nil(t, empty)
}

func TestCompileExtraHTTPSOriginsRejectsInvalid(t *testing.T) {
	for _, origin := range []string{
		"", "pypi.org", "http://pypi.org", "https://*.pypi.org", "https://pypi.org:80", "https://pypi.org:0443", "https://pypi.org:",
		"https://127.0.0.1", "https://[::1]", "https://user:pass@pypi.org", "https://pypi.org/simple", "https://pypi.org//",
		"https://pypi.org/%2f", "https://pypi.org?", "https://pypi.org?q=x", "https://pypi.org#", "https://pypi.org#x",
		" https://pypi.org", "https://pypi.org ", "https://pypi.org.", "https://localhost", "https://-pypi.org", "https://pypi_.org",
		"https://" + strings.Repeat("a", 64) + ".org",
	} {
		t.Run(origin, func(t *testing.T) {
			destinations, err := translator.CompileExtraHTTPSOrigins([]string{origin})
			require.Error(t, err)
			require.Nil(t, destinations)
			require.ErrorAs(t, err, new(*translator.ValidationError))
		})
	}
	_, err := translator.CompileExtraHTTPSOrigins(make([]string, 33))
	require.ErrorContains(t, err, "at most 32")
}

func TestExtraHTTPSOriginsRejectCredentialOverlap(t *testing.T) {
	for _, header := range []string{"authorization", "x-api-key", "x-mcp-token"} {
		err := translator.ValidateExtraHTTPSCredentials([]string{"https://pypi.org:443"}, []egress.Credential{{Hostname: "pypi.org", Header: header}})
		require.ErrorContains(t, err, "already has a credential binding")
	}
	require.NoError(t, translator.ValidateExtraHTTPSCredentials([]string{"https://pypi.org:443"}, []egress.Credential{{Hostname: "api.openai.com", Header: "authorization"}}))
}
