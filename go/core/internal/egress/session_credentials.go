package egress

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"buf.build/go/protovalidate"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// SessionCredentials validates per-session references against immutable revision
// inputs and merges them in canonical order. Secrets hold complete header values.
// Host/header bindings cannot override revision bindings, even with the same URI.
func SessionCredentials(namespace string, requested []*apiv1alpha1.SessionCredential, destinations []string, revision []Credential) ([]Credential, error) {
	if len(requested) > 4 {
		return nil, fmt.Errorf("at most four Session credentials are allowed")
	}
	result := slices.Clone(revision)
	occupied := map[string]bool{}
	for _, binding := range revision {
		occupied[strings.ToLower(strings.TrimSuffix(binding.Hostname, "."))+"\x00"+strings.ToLower(binding.Header)] = true
	}
	allowed := map[string]bool{}
	for _, destination := range destinations {
		u, err := url.Parse(destination)
		if err != nil {
			return nil, fmt.Errorf("invalid revision destination")
		}
		allowed[Origin(u)] = true
	}
	for _, binding := range requested {
		if binding == nil {
			return nil, fmt.Errorf("nil Session credential")
		}
		if err := protovalidate.Validate(binding); err != nil {
			return nil, fmt.Errorf("invalid Session credential: %w", err)
		}
		u, err := url.Parse(binding.GetOrigin())
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return nil, fmt.Errorf("session credential requires an HTTP(S) origin")
		}
		if !allowed[Origin(u)] {
			return nil, fmt.Errorf("session credential origin is not allowed by the revision")
		}
		ref := binding.GetSecretRef()
		credential := Credential{Hostname: u.Hostname(), Header: binding.GetHeader(), URI: "ate-secret://k8s.io/default/" + namespace + "/" + ref.GetName() + "/" + ref.GetKey()}
		canonical, err := CanonicalCredentials([]Credential{credential})
		if err != nil {
			return nil, err
		}
		credential = canonical[0]
		key := credential.Hostname + "\x00" + credential.Header
		if occupied[key] {
			return nil, fmt.Errorf("duplicate Session or revision credential for host and header")
		}
		occupied[key] = true
		result = append(result, credential)
	}
	return CanonicalCredentials(result)
}
