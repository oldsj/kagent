package utils_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/stretchr/testify/require"
)

func TestSafeDiagnostic(t *testing.T) {
	for _, input := range []string{
		"Authorization: Basic Q7m9v2R8d4",
		"API\nkey is Q7m9v2R8d4",
		"-----BEGIN PRIVATE\u00a0KEY-----\nQ7m9v2R8d4",
		"Authori\nzation: Q7m9v2R8d4",
		"ſecret: Q7m9v2R8d4",
		"API Error: upstream echoed Authorization: \"Custom prefix\nQ7m9v2R8d4\"",
		"Authorization: Digest username=alice, response=Q7m9v2R8d4",
		`"Authorization": "Custom Q7m9v2R8d4"`,
		"bearer Q7m9v2R8d4",
		`Bearer "Q7m9v2R8d4"`,
		"authorization",
		"API Error: 401 invalid x-api-key",
		"x-api-key: Q7m9v2R8d4",
		"API KEY",
		"api-key",
		"apikey",
		"Cookie: Q7m9v2R8d4",
		"Set-Cookie: Q7m9v2R8d4",
		"-----BEGIN PRIVATE KEY-----\nQ7m9v2R8d4",
		"-----BEGIN RSA PRIVATE KEY-----\nQ7m9v2R8d4",
		strings.Repeat("clean diagnostic ", 200) + "ToKeN=Q7m9v2R8d4",
		"sk-Q7m9v2R8d4",
		`"api_key": "Q7m9v2R8d4 with spaces"`,
		`"api_key": "escaped\"Q7m9v2R8d4"`,
		"password: \"multiline\nQ7m9v2R8d4\"",
		"client_secret='Q7m9v2R8d4 with spaces'",
		"token=Q7m9v2R8d4",
		"password: Q7m9v2R8d4 with spaces",
		"key=Q7m9v2R8d4",
		"secret: Q7m9v2R8d4",
		"API_KEY=" + strings.Repeat("Q7m9v2R8d4", 200),
	} {
		t.Run(input[:min(len(input), 50)], func(t *testing.T) {
			got := utils.SafeDiagnostic(input)
			require.NotContains(t, got, "Q7m9v2R8d4")
			require.Equal(t, "upstream error (details withheld: possible credential)", got)
			require.Equal(t, got, utils.SafeDiagnostic(got))
			require.LessOrEqual(t, len(got), utils.MaxDiagnosticBytes)
		})
	}
	message := "API Error: 503 upstream unavailable"
	require.Equal(t, message, utils.SafeDiagnostic(message))
	got := utils.SafeDiagnostic(strings.Repeat("界", 1000))
	require.True(t, utf8.ValidString(got))
	require.LessOrEqual(t, len(got), utils.MaxDiagnosticBytes)
	require.True(t, strings.HasSuffix(got, "..."))
	require.Equal(t, strings.Repeat("界", 340)+"...", got)
}

func TestSafeDiagnosticSplitIndicators(t *testing.T) {
	for _, indicator := range []string{"authorization", "bearer", "apikey", "token", "secret", "password", "cookie", "privatekey"} {
		t.Run(indicator, func(t *testing.T) {
			var input strings.Builder
			for _, letter := range strings.ToUpper(indicator) {
				input.WriteRune(letter)
				input.WriteString("\n\u00a0-_.012")
			}
			input.WriteString(": Q7m9v2R8d4")
			require.Equal(t, "upstream error (details withheld: possible credential)", utils.SafeDiagnostic(input.String()))
		})
	}
}
