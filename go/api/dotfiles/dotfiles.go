// Package dotfiles holds the rules shared by the control plane and the Harness
// runtimes for a Session's agent dotfiles bundle: which Session credential
// authorizes its download, and the limits a bundle must stay within.
package dotfiles

import (
	"errors"
	"net/url"
	"strings"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
)

// Limits a bundle must stay within. The producer enforces the same ones; the
// runtime checks them again before laying anything out.
const (
	MaxBundleBytes = 32 << 20
	MaxFiles       = 4000
	MaxFileBytes   = 2 << 20
	MaxTotalBytes  = 16 << 20
)

// ErrNoCredential means no Session credential is bound to the bundle url's origin.
var ErrNoCredential = errors.New("dotfiles url origin carries none of the Session's credentials")

// Origin is rawURL's "scheme://host[:port]", lower-cased, as a SessionCredential
// names it. Only http and https urls without user info, query or fragment qualify.
func Origin(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("dotfiles url must be an http or https url without credentials, query or fragment")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// Credential returns the Session credential whose origin is the bundle url's.
func Credential(d *apiv1.Dotfiles, credentials []*apiv1.SessionCredential) (*apiv1.SessionCredential, error) {
	origin, err := Origin(d.GetUrl())
	if err != nil {
		return nil, err
	}
	for _, c := range credentials {
		if strings.EqualFold(c.GetOrigin(), origin) {
			return c, nil
		}
	}
	return nil, ErrNoCredential
}
