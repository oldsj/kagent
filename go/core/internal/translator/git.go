package translator

import (
	"net/url"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
)

// CompileGit returns the runtime's Git policy and the egress origins it needs.
// A nil spec returns nil values, so a Harness without git compiles exactly as
// it did before the field existed.
func CompileGit(spec *v1alpha3.HarnessGit) (*workspace.Git, []string) {
	if spec == nil {
		return nil, nil
	}
	policy := &workspace.Git{Origins: make([]string, 0, len(spec.Origins)), Credential: spec.CredentialSecretRef != nil}
	destinations := make([]string, 0, len(spec.Origins))
	for _, origin := range spec.Origins {
		origin = workspace.NormalizeHost(origin)
		policy.Origins = append(policy.Origins, origin)
		destinations = append(destinations, egress.Origin(&url.URL{Scheme: "https", Host: origin}))
	}
	return policy, destinations
}

// GitOrigins returns the policy's origins, or nil when Sessions cannot request
// a workspace.
func GitOrigins(policy *workspace.Git) []string {
	if policy == nil {
		return nil
	}
	return policy.Origins
}
