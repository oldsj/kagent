package egress

import (
	"fmt"
	"slices"
	"strings"

	"buf.build/go/protovalidate"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

// SessionCredentials validates per-session references against the access the
// Session's revision already has and returns that access's credentials with the
// Session's added, in canonical order. Secrets hold complete header values. A
// Session binding may only target an origin the access already allows, and it
// cannot replace an existing binding for the same origin and header, even with
// the same Secret.
func SessionCredentials(namespace string, requested []*apiv1.SessionCredential, destinations []string, existing []Credential) ([]Credential, error) {
	if len(requested) > 4 {
		return nil, fmt.Errorf("at most four Session credentials are allowed")
	}
	result := slices.Clone(existing)
	occupied := map[string]bool{}
	for _, binding := range existing {
		origin, err := ParseOrigin(binding.Origin)
		if err != nil {
			return nil, fmt.Errorf("invalid revision credential origin")
		}
		occupied[origin.String()+"\x00"+strings.ToLower(binding.Header)] = true
	}
	allowed := map[string]bool{}
	for _, destination := range destinations {
		origin, err := ParseOrigin(destination)
		if err != nil {
			// Wildcard and non-HTTP destinations cannot carry a credential.
			continue
		}
		allowed[origin.String()] = true
	}
	for _, binding := range requested {
		if binding == nil {
			return nil, fmt.Errorf("nil Session credential")
		}
		if err := protovalidate.Validate(binding); err != nil {
			return nil, fmt.Errorf("invalid Session credential: %w", err)
		}
		origin, err := ParseOrigin(binding.GetOrigin())
		if err != nil {
			return nil, fmt.Errorf("session credential requires an HTTP(S) origin")
		}
		if !allowed[origin.String()] {
			return nil, fmt.Errorf("session credential origin is not allowed by the revision")
		}
		ref := binding.GetSecretRef()
		canonical, err := CanonicalCredentials([]Credential{{
			Origin: origin.String(), Header: binding.GetHeader(),
			URI: "ate-secret://k8s.io/default/" + namespace + "/" + ref.GetName() + "/" + ref.GetKey(),
		}})
		if err != nil {
			return nil, err
		}
		credential := canonical[0]
		key := credential.Origin + "\x00" + credential.Header
		if occupied[key] {
			return nil, fmt.Errorf("duplicate Session or revision credential for origin and header")
		}
		occupied[key] = true
		result = append(result, credential)
	}
	return CanonicalCredentials(result)
}
