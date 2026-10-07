package utils

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

// MaxDiagnosticBytes bounds each diagnostic exposed in logs or task status.
const MaxDiagnosticBytes = 1024

var diagnosticCredentials = regexp.MustCompile(`(?i)sk-|\bkey["']?\s*[:=]`)

// SafeDiagnostic withholds the entire diagnostic when a credential indicator
// appears anywhere, before truncation. Arbitrary upstream text cannot be safely
// scrubbed by parsing values. Detection case-folds Unicode and removes all
// non-letter runes, so wrapping, punctuation, and Unicode whitespace cannot
// split indicators. False positives are acceptable. Display text is unchanged.
// Even bare header names (such as "invalid x-api-key")
// are withheld: they do not establish that the rest of the text is safe.
// It is for failure diagnostics, not arbitrary request or response payloads.
func SafeDiagnostic(message string) string {
	letters := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return r
		}
		return -1
	}, cases.Fold().String(message))
	for _, indicator := range []string{"authorization", "bearer", "apikey", "token", "secret", "password", "cookie", "privatekey"} {
		if strings.Contains(letters, indicator) {
			return "upstream error (details withheld: possible credential)"
		}
	}
	if diagnosticCredentials.MatchString(message) {
		return "upstream error (details withheld: possible credential)"
	}
	message = strings.TrimSpace(message)
	if len(message) > MaxDiagnosticBytes {
		const suffix = "..."
		message = message[:MaxDiagnosticBytes-len(suffix)]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += suffix
	}
	return message
}
