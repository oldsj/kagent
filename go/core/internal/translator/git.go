package translator

import (
	"net/url"
	"strings"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
)

// CompileGit returns the runtime's Git policy and the egress origins it needs.
// A nil spec adds no Git configuration or destinations.
func CompileGit(spec *v1alpha3.HarnessGit) (*workspace.Git, []string, error) {
	if spec == nil {
		return nil, nil, nil
	}
	policy := &workspace.Git{Origins: append([]string(nil), spec.Origins...), Credential: spec.CredentialSecretRef != nil}
	if spec.ReadProxyOrigin == nil && spec.PushProxyOrigin == nil {
		for i, origin := range policy.Origins {
			policy.Origins[i] = workspace.NormalizeHost(origin)
		}
	}
	if spec.ReadProxyOrigin != nil {
		policy.ReadProxyOrigin = new(*spec.ReadProxyOrigin)
	}
	if spec.PushProxyOrigin != nil {
		policy.PushProxyOrigin = new(*spec.PushProxyOrigin)
	}
	if err := policy.Validate(); err != nil {
		return nil, nil, NewValidationError("invalid Git policy: %v", err)
	}
	if policy.ReadProxyOrigin != nil {
		destinations := []string{egress.Origin(mustProxyURL(*policy.ReadProxyOrigin))}
		if policy.PushProxyOrigin != nil {
			destinations = append(destinations, egress.Origin(mustProxyURL(*policy.PushProxyOrigin)))
		}
		return policy, destinations, nil
	}
	destinations := make([]string, 0, len(spec.Origins))
	for _, origin := range policy.Origins {
		destinations = append(destinations, egress.Origin(&url.URL{Scheme: "https", Host: origin}))
	}
	return policy, destinations, nil
}

// mustProxyURL is used only after exact proxy-origin validation.
func mustProxyURL(origin string) *url.URL {
	u, _ := url.Parse(origin)
	return u
}

func gitProxyHost(host string) bool {
	return host == mustProxyURL(workspace.ReadProxyOrigin).Hostname() || host == mustProxyURL(workspace.PushProxyOrigin).Hostname()
}

func directGitHubHost(host string) bool {
	for _, domain := range []string{"github.com", "githubusercontent.com", "githubassets.com", "github.io"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// ValidateGitEgress checks the complete native revision, after every inherited
// contributor. Explicit standalone direct Git is retained; proxy and no-Git
// revisions cannot acquire direct GitHub or legacy PAT authority sideways.
func ValidateGitEgress(policy *workspace.Git, destinations []string, credentials []egress.Credential) error {
	if policy != nil {
		if err := policy.Validate(); err != nil {
			return NewValidationError("invalid Git policy: %v", err)
		}
		if policy.ReadProxyOrigin == nil {
			return nil
		}
	}
	for _, destination := range destinations {
		u, err := url.Parse(destination)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return NewValidationError("invalid effective egress origin")
		}
		host := workspace.NormalizeHost(u.Hostname())
		if directGitHubHost(host) {
			return NewValidationError("effective egress contains direct GitHub authority")
		}
		if gitProxyHost(host) {
			allowed := policy != nil && (egress.Origin(u) == workspace.ReadProxyOrigin+":80" || (policy.PushProxyOrigin != nil && egress.Origin(u) == workspace.PushProxyOrigin+":80"))
			if !allowed {
				return NewValidationError("effective egress contains an unapproved Git proxy origin")
			}
		}
	}
	for _, credential := range credentials {
		u, err := url.Parse(credential.URI)
		if err != nil {
			return NewValidationError("invalid effective credential reference")
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		host := workspace.NormalizeHost(credential.Hostname)
		if directGitHubHost(host) || gitProxyHost(host) || (len(parts) == 4 && parts[2] == "mainloop-git-auth") {
			return NewValidationError("effective revision contains a reserved Git credential; Git proxies require per-Session references")
		}
	}
	return nil
}

// GitOrigins returns the policy's origins, or nil when Sessions cannot request
// a workspace.
func GitOrigins(policy *workspace.Git) []string {
	if policy == nil {
		return nil
	}
	return policy.Origins
}
