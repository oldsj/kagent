package egress

import (
	"net/url"
	"slices"
	"strings"

	"github.com/kagent-dev/kagent/go/api/workspace"
)

// FilterGitRestricted removes from live grants everything a Git-restricted
// revision must not gain after compilation: destinations on GitHub host
// families or on the trusted Git proxy hosts (wildcards included), credentials
// bound to those hosts, and the legacy Git token Secret on any host. The
// revision's own compiled access is not passed through here; it already carries
// exactly the Git access its profile allows. Inputs are not modified. Dropped
// entries are returned sorted for reporting.
func FilterGitRestricted(grants Access) (Access, []string) {
	var kept Access
	var dropped []string
	for _, destination := range grants.Destinations {
		if gitRestrictedOrigin(destination) {
			dropped = append(dropped, destination)
			continue
		}
		kept.Destinations = append(kept.Destinations, destination)
	}
	for _, credential := range grants.Credentials {
		if gitRestrictedOrigin(credential.Origin) || legacyGitCredential(credential.URI) {
			dropped = append(dropped, credential.Origin+" "+credential.Header)
			continue
		}
		kept.Credentials = append(kept.Credentials, credential)
	}
	slices.Sort(dropped)
	return kept, slices.Compact(dropped)
}

// gitRestrictedOrigin fails closed: an origin that cannot be parsed is treated
// as restricted rather than passed to the Actor.
func gitRestrictedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := workspace.NormalizeHost(u.Hostname())
	// A leftmost-label wildcard covers every host under its suffix, so it is
	// restricted when that suffix reaches GitHub or any Git proxy listener.
	if suffix, ok := strings.CutPrefix(host, "*."); ok {
		return workspace.DirectGitHubHost(suffix) || workspace.CoversGitProxyHost(suffix)
	}
	return workspace.DirectGitHubHost(host) || workspace.IsGitProxyHost(host)
}

func legacyGitCredential(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return uri != ""
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	return len(parts) == 4 && parts[2] == workspace.LegacyGitCredentialSecret
}
