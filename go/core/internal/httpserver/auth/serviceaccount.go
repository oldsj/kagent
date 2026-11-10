package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/sync/singleflight"
	typedauth "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/utils/lru"

	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

const serviceAccountAudience = "kagent-invocation"

// ServiceAccountProvider verifies Kubernetes credentials independently of the
// user provider. Unverified claims only select this verifier, never authority.
type ServiceAccountProvider struct {
	users     auth.AuthProvider
	reviews   typedauth.TokenReviewInterface
	authority string
	// allowed is nil when every verified service account may call.
	allowed  map[string]struct{}
	verified *lru.Cache
	lookups  singleflight.Group
}

// NewServiceAccountProvider verifies service-account callers and passes every
// other credential to users. A non-empty allowedSubjects admits only those
// verified usernames (system:serviceaccount:<namespace>:<name>).
func NewServiceAccountProvider(users auth.AuthProvider, reviews typedauth.TokenReviewInterface, authority string, allowedSubjects ...string) *ServiceAccountProvider {
	var allowed map[string]struct{}
	if len(allowedSubjects) != 0 {
		allowed = make(map[string]struct{}, len(allowedSubjects))
		for _, subject := range allowedSubjects {
			allowed[subject] = struct{}{}
		}
	}
	return &ServiceAccountProvider{users: users, reviews: reviews, authority: authority, allowed: allowed, verified: lru.New(tokenReviewCacheSize)}
}

// ServiceAccountOnlyAuthenticator is the user provider for service-account
// mode: no user credential is accepted, so only callers the service-account
// and runtime Actor verifiers establish can authenticate.
type ServiceAccountOnlyAuthenticator struct{}

var _ auth.AuthProvider = ServiceAccountOnlyAuthenticator{}

func (ServiceAccountOnlyAuthenticator) Authenticate(context.Context, http.Header, url.Values) (auth.Session, error) {
	return nil, ErrUnauthenticated
}

func (ServiceAccountOnlyAuthenticator) UpstreamAuth(r *http.Request, session auth.Session, _ string) error {
	return auth.ForwardCredential(r, session)
}

func (p *ServiceAccountProvider) Authenticate(ctx context.Context, headers http.Header, query url.Values) (auth.Session, error) {
	selected := false
	var claims map[string]any
	for _, header := range headers.Values("Authorization") {
		_, token, _ := strings.Cut(header, " ")
		claims, _ = parseJWTPayload(token)
		subject, _ := claims["sub"].(string)
		_, kubernetes := claims["kubernetes.io"]
		selected = selected || kubernetes || strings.HasPrefix(subject, "system:serviceaccount:")
	}
	if !selected {
		return p.users.Authenticate(ctx, headers, query)
	}
	if len(headers.Values("Authorization")) != 1 {
		return nil, ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(headers.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, ErrUnauthenticated
	}
	principal, err := p.verify(ctx, token, serviceAccountAudience)
	if err != nil {
		return nil, err
	}
	expiresAt, err := jwtExpiration(claims)
	if err != nil {
		return nil, err
	}
	// This immutable credential was just verified for this request. Forwarding
	// checks its expiry locally without another lookup.
	source := bearerCredential{authHeader: headers.Get("Authorization"), expiresAt: expiresAt}
	return auth.NewCredentialSession(principal, source), nil
}

func (p *ServiceAccountProvider) UpstreamAuth(request *http.Request, session auth.Session, destinationSessionID string) error {
	return p.users.UpstreamAuth(request, session, destinationSessionID)
}
