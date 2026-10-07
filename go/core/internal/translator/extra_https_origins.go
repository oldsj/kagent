package translator

import (
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"k8s.io/apimachinery/pkg/util/validation"
)

// CompileExtraHTTPSOrigins validates trusted, credential-free whole-host grants
// and uses the same destination representation as the other egress contributors.
func CompileExtraHTTPSOrigins(origins []string) ([]string, error) {
	if len(origins) > 32 {
		return nil, NewValidationError("extraHTTPSOrigins must contain at most 32 origins")
	}
	var destinations []string
	for _, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil || len(origin) > 273 || !strings.HasPrefix(origin, "https://") || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
			return nil, NewValidationError("extraHTTPSOrigins entry must be an exact HTTPS DNS origin without credentials, query, fragment, or non-root path")
		}
		host := strings.ToLower(u.Hostname())
		_, ipErr := netip.ParseAddr(host)
		if ipErr == nil || len(validation.IsDNS1123Subdomain(host)) != 0 || !strings.Contains(host, ".") || (u.Host != u.Hostname() && u.Host != u.Hostname()+":443") {
			return nil, NewValidationError("extraHTTPSOrigins entry must use an exact DNS hostname and port 443")
		}
		labels := strings.Split(host, ".")
		if last := labels[len(labels)-1]; last[0] < 'a' || last[0] > 'z' {
			return nil, NewValidationError("extraHTTPSOrigins must use a DNS name with an alphabetic top-level label")
		}
		for _, label := range labels {
			if len(label) > 63 {
				return nil, NewValidationError("extraHTTPSOrigins DNS labels must contain at most 63 characters")
			}
		}
		destinations = append(destinations, egress.Origin(&url.URL{Scheme: "https", Host: host}))
	}
	slices.Sort(destinations)
	return slices.Compact(destinations), nil
}

// ValidateExtraHTTPSCredentials prevents existing provider, Git, or MCP bindings
// from giving an extra origin credential effects through a shared hostname.
func ValidateExtraHTTPSCredentials(destinations []string, credentials []egress.Credential) error {
	for _, credential := range credentials {
		destination := egress.Origin(&url.URL{Scheme: "https", Host: credential.Hostname})
		if slices.Contains(destinations, destination) {
			return NewValidationError("extraHTTPSOrigins host %q already has a credential binding", credential.Hostname)
		}
	}
	return nil
}
