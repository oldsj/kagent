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

var (
	// A URL can carry userinfo or a signed query, so the whole URL is withheld.
	diagnosticURL   = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>()\[\]{}]+`)
	diagnosticEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+`)
	diagnosticUUID  = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// Opaque words are candidates for unlabelled credentials such as JWTs.
	diagnosticOpaque = regexp.MustCompile(`[A-Za-z0-9_.+/=-]{8,}`)
)

// redactIdentifiers replaces values that identify an account or may grant
// access, which carry no indicator for SafeDiagnostic to withhold on. The
// replacements contain nothing that a second pass would redact again.
func redactIdentifiers(message string) string {
	message = diagnosticURL.ReplaceAllString(message, "[url]")
	message = diagnosticEmail.ReplaceAllString(message, "[email]")
	message = diagnosticUUID.ReplaceAllString(message, "[id]")
	return diagnosticOpaque.ReplaceAllStringFunc(message, func(word string) string {
		if opaque(word) {
			return "[redacted]"
		}
		return word
	})
}

// opaque reports a word that looks generated rather than written: mixed-case
// letters with digits, as in tokens and signatures, or a long run of letters
// and digits. Model names such as gpt-5.5-codex and error codes stay readable.
func opaque(word string) bool {
	var upper, lower, digit bool
	for _, r := range word {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return (upper && lower && digit) || (len(word) >= 32 && digit && (upper || lower))
}

// SafeDiagnostic withholds the entire diagnostic when a credential indicator
// appears anywhere, before truncation. Arbitrary upstream text cannot be safely
// scrubbed by parsing values. Detection case-folds Unicode and removes all
// non-letter runes, so wrapping, punctuation, and Unicode whitespace cannot
// split indicators. False positives are acceptable. Otherwise the text is kept,
// except that URLs, email addresses, UUIDs and opaque token-like words are
// replaced, since they can identify an account or grant access unlabelled.
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
	message = strings.TrimSpace(redactIdentifiers(message))
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
