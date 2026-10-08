package taskstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

type bindingStore interface {
	GetRuntimeGenerationByDigest(context.Context, []byte) (*database.RuntimeGeneration, error)
}

// Authenticator accepts a single bounded capability overwritten by the trusted
// gateway. Actor metadata and control-plane bearer credentials carry no authority.
type Authenticator struct{ Store bindingStore }

var _ auth.AuthProvider = (*Authenticator)(nil)

func (a *Authenticator) Authenticate(ctx context.Context, headers http.Header, _ url.Values) (auth.Session, error) {
	for _, forbidden := range []string{"authorization", "x-user-id", "x-agent-name", "x-kagent-insecure-runtime-identity"} {
		if len(headers.Values(forbidden)) != 0 {
			return nil, fmt.Errorf("invalid runtime credential combination")
		}
	}
	values := headers.Values(egress.RuntimeTokenHeader)
	if len(values) != 1 || len(values[0]) != 64 {
		return nil, fmt.Errorf("invalid runtime capability")
	}
	token, err := hex.DecodeString(values[0])
	if err != nil || len(token) != 32 || hex.EncodeToString(token) != values[0] || a.Store == nil {
		return nil, fmt.Errorf("invalid runtime capability")
	}
	digest := sha256.Sum256([]byte(values[0]))
	binding, err := a.Store.GetRuntimeGenerationByDigest(ctx, digest[:])
	if err != nil || binding == nil || binding.Phase != "active" || binding.ActorUID == "" {
		return nil, fmt.Errorf("invalid runtime capability")
	}
	return runtimeSession{binding: *binding}, nil
}

func (*Authenticator) UpstreamAuth(*http.Request, auth.Session, auth.Principal) error {
	return fmt.Errorf("runtime authentication cannot forward public credentials")
}

type runtimeSession struct{ binding database.RuntimeGeneration }

func (s runtimeSession) Principal() auth.Principal {
	return auth.Principal{Agent: auth.Agent{ID: s.binding.SessionID.String()}}
}
