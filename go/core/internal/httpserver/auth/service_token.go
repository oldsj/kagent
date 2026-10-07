package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kagent-dev/kagent/go/core/internal/service/controlauth"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"k8s.io/apimachinery/pkg/types"
)

const maxServiceTokenBytes = 4096

var errServiceToken = errors.New("invalid service credentials or token configuration")

// ServiceTokenAuthenticator rereads bounded files for each request. A failed
// reload denies the request instead of retaining a revoked credential.
// Sessions contain only the fixed principal, never the bearer.
type ServiceTokenAuthenticator struct {
	current, next string
	policy        *controlauth.Policy
}

var _ auth.AuthProvider = (*ServiceTokenAuthenticator)(nil)

func NewServiceTokenAuthenticator(current, next string, policy *controlauth.Policy) (*ServiceTokenAuthenticator, error) {
	if policy == nil {
		return nil, errServiceToken
	}
	a := &ServiceTokenAuthenticator{current: current, next: next, policy: policy}
	if _, err := a.tokens(); err != nil {
		return nil, err
	}
	return a, nil
}

func readServiceToken(path string) ([32]byte, error) {
	var zero [32]byte
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return zero, errServiceToken
	}
	f, err := os.Open(path)
	if err != nil {
		return zero, errServiceToken
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return zero, errServiceToken
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxServiceTokenBytes+1))
	if err != nil || len(raw) > maxServiceTokenBytes {
		return zero, errServiceToken
	}
	// Secret files commonly have one trailing newline. Other whitespace is invalid.
	token := strings.TrimSuffix(string(raw), "\n")
	if !validServiceToken(token) {
		return zero, errServiceToken
	}
	return sha256.Sum256([]byte(token)), nil
}

func (a *ServiceTokenAuthenticator) tokens() ([][32]byte, error) {
	current, err := readServiceToken(a.current)
	if err != nil {
		return nil, err
	}
	tokens := [][32]byte{current}
	if a.next != "" {
		next, err := readServiceToken(a.next)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, next)
	}
	return tokens, nil
}

func (a *ServiceTokenAuthenticator) Authenticate(_ context.Context, headers http.Header, _ url.Values) (auth.Session, error) {
	var values []string
	for key, v := range headers {
		if strings.EqualFold(key, "Authorization") {
			values = append(values, v...)
		}
	}
	if len(values) != 1 {
		return nil, errServiceToken
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validServiceToken(token) {
		return nil, errServiceToken
	}
	tokens, err := a.tokens()
	if err != nil {
		return nil, err
	}
	supplied := sha256.Sum256([]byte(token))
	matched := 0
	for _, expected := range tokens {
		matched |= subtle.ConstantTimeCompare(supplied[:], expected[:])
	}
	if matched != 1 {
		return nil, errServiceToken
	}
	return &serviceSession{policy: a.policy}, nil
}

func (*ServiceTokenAuthenticator) UpstreamAuth(r *http.Request, session auth.Session, _ auth.Principal) error {
	for key := range r.Header {
		if strings.EqualFold(key, "Authorization") {
			delete(r.Header, key)
		}
	}
	if session != nil {
		r.Header.Set("X-User-Id", session.Principal().User.ID)
	}
	return nil
}

// serviceSession carries trusted policy, never incoming identity or credentials.
type serviceSession struct{ policy *controlauth.Policy }

func (*serviceSession) Principal() auth.Principal {
	return auth.Principal{User: auth.User{ID: auth.MainloopService}, Service: auth.MainloopService}
}
func (s *serviceSession) CheckAgent(ctx context.Context, ref types.NamespacedName) error {
	return s.policy.Check(ctx, s.Principal(), auth.VerbGet, auth.Resource{Type: "Agent", Namespace: ref.Namespace, Name: ref.Name})
}

func validServiceToken(token string) bool {
	if len(token) < 32 || len(token) > maxServiceTokenBytes || token[0] == '=' {
		return false
	}
	padding := false
	for _, ch := range token {
		if ch == '=' {
			padding = true
			continue
		}
		if padding {
			return false
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._~+/", ch) {
			continue
		}
		return false
	}
	return true
}

// Authorizer returns the policy validated with this service's credentials.
func (a *ServiceTokenAuthenticator) Authorizer() auth.CollectionAuthorizer { return a.policy }
