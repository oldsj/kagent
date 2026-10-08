// Package workspace holds the pure rules shared by the control plane and the
// Harness runtimes for bootstrapping a Git workspace: which repository URLs are
// acceptable and how a repository maps to an allowed Git origin.
package workspace

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultDepth is the shallow-clone depth used when a request names none.
	DefaultDepth = 1
	// MaxDepth bounds shallow clones so a typo cannot request unbounded history.
	MaxDepth = 1000
	// ReadProxyOrigin and PushProxyOrigin are the exact trusted Git listeners.
	// Their method, repository and capability checks belong to Mainloop.
	ReadProxyOrigin = "http://mainloop-git-read.mainloop.svc.cluster.local"
	PushProxyOrigin = "http://mainloop-git-push.mainloop.svc.cluster.local"
)

// Git is the compiled, non-secret Git policy of a Harness revision. The runtime
// refuses to clone from hosts outside Origins even if the control plane
// accepted the request.
type Git struct {
	Origins []string `json:"origins"`
	// Credential reports that the egress gateway injects an Authorization header
	// for the single origin. The runtime then sends a placeholder header so the
	// gateway has something to overwrite; without it the clone is anonymous.
	Credential      bool    `json:"credential,omitempty"`
	ReadProxyOrigin *string `json:"readProxyOrigin,omitempty"`
	PushProxyOrigin *string `json:"pushProxyOrigin,omitempty"`
}

// Validate rejects a policy the runtime cannot enforce.
func (g Git) Validate() error {
	if len(g.Origins) == 0 {
		return fmt.Errorf("git origins are required")
	}
	for _, origin := range g.Origins {
		if err := validateHost(origin); err != nil {
			return fmt.Errorf("git origin %q: %w", origin, err)
		}
	}
	if g.Credential && len(g.Origins) != 1 {
		return fmt.Errorf("a git credential requires exactly one origin")
	}
	if g.PushProxyOrigin != nil && g.ReadProxyOrigin == nil {
		return fmt.Errorf("push proxy requires a read proxy")
	}
	if g.ReadProxyOrigin != nil {
		if *g.ReadProxyOrigin != ReadProxyOrigin || (g.PushProxyOrigin != nil && *g.PushProxyOrigin != PushProxyOrigin) {
			return fmt.Errorf("git proxy origins must be the exact trusted listeners")
		}
		if len(g.Origins) != 1 || g.Origins[0] != "github.com" || g.Credential {
			return fmt.Errorf("git proxies require canonical github.com identity and no legacy credential")
		}
	}
	return nil
}

var repositoryComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Transport returns fetch and push URLs without changing the canonical Session
// identity. In proxy mode only a GitHub owner/repository can select a path.
func (g Git) Transport(repo string) (readURL, pushURL string, err error) {
	if err := g.Validate(); err != nil {
		return "", "", err
	}
	host, err := RepoHost(repo)
	if err != nil || !g.Allows(host) {
		return "", "", fmt.Errorf("repository is outside the Git identity policy")
	}
	if g.ReadProxyOrigin == nil {
		return repo, "", nil
	}
	u, err := url.Parse(repo)
	if err != nil || !strings.EqualFold(u.Host, "github.com") || u.RawPath != "" || strings.ContainsAny(repo, "#%\\") {
		return "", "", fmt.Errorf("proxy repository must be canonical HTTPS GitHub owner/repository")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("proxy repository requires exactly owner/repository")
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	for _, part := range parts {
		if !repositoryComponent.MatchString(part) {
			return "", "", fmt.Errorf("proxy repository has an invalid component")
		}
	}
	path := "/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]) + ".git"
	readURL = *g.ReadProxyOrigin + path
	pushURL = readURL
	if g.PushProxyOrigin != nil {
		pushURL = *g.PushProxyOrigin + path
	}
	return readURL, pushURL, nil
}

// Allows reports whether host is one of the origins.
func (g Git) Allows(host string) bool {
	host = NormalizeHost(host)
	for _, origin := range g.Origins {
		if NormalizeHost(origin) == host {
			return true
		}
	}
	return false
}

// NormalizeHost lowercases a host and drops a trailing dot.
func NormalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// RepoHost returns the host of an HTTPS repository URL. Credentials, ports,
// queries, and fragments are rejected: authentication belongs to the egress
// gateway, so a URL carrying a token must never be stored or logged.
func RepoHost(repo string) (string, error) {
	u, err := url.Parse(repo)
	if err != nil {
		return "", fmt.Errorf("repo is not a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("repo must be an https URL")
	case u.User != nil:
		return "", fmt.Errorf("repo must not contain credentials")
	case u.Port() != "":
		return "", fmt.Errorf("repo must not specify a port")
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return "", fmt.Errorf("repo must not contain a query or fragment")
	case strings.Trim(u.Path, "/") == "":
		return "", fmt.Errorf("repo must include a path")
	}
	host := NormalizeHost(u.Hostname())
	if err := validateHost(host); err != nil {
		return "", fmt.Errorf("repo host: %w", err)
	}
	return host, nil
}

func validateHost(host string) error {
	if _, err := netip.ParseAddr(host); err == nil {
		return fmt.Errorf("must be a DNS name, not an IP address")
	}
	if host != NormalizeHost(host) || len(validation.IsDNS1123Subdomain(host)) != 0 || !strings.Contains(host, ".") {
		return fmt.Errorf("must be a lowercase DNS name")
	}
	return nil
}
