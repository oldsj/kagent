package taskstore

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/stretchr/testify/require"
)

type authBindings struct {
	binding database.RuntimeGeneration
	reads   int
}

func (s *authBindings) GetRuntimeGenerationByDigest(_ context.Context, digest []byte) (*database.RuntimeGeneration, error) {
	s.reads++
	if !equalBytes(digest, s.binding.TokenDigest) || s.binding.Phase != "active" {
		return nil, database.ErrNotFound
	}
	return &s.binding, nil
}
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRuntimeCapabilityHeader(t *testing.T) {
	token := strings.Repeat("ab", 32)
	digest := sha256.Sum256([]byte(token))
	for _, test := range []struct {
		name    string
		values  []string
		revoked bool
		valid   bool
	}{
		{"active", []string{token}, false, true},
		{"missing", nil, false, false}, {"duplicate", []string{token, token}, false, false},
		{"comma joined", []string{token + "," + token}, false, false},
		{"placeholder", []string{"gateway-injection-required"}, false, false},
		{"short", []string{"ab"}, false, false}, {"malformed", []string{strings.Repeat("z", 64)}, false, false},
		{"uppercase", []string{strings.ToUpper(token)}, false, false}, {"random", []string{strings.Repeat("cd", 32)}, false, false},
		{"revoked", []string{token}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := database.RuntimeGeneration{ID: uuid.New(), SessionID: uuid.New(), Atespace: "team-a", ActorName: "issued-name", ActorUID: "issued-uid", TokenDigest: digest[:], Phase: "active"}
			if test.revoked {
				binding.Phase = "revoked"
			}
			store := &authBindings{binding: binding}
			headers := http.Header{}
			for _, value := range test.values {
				headers.Add(egress.RuntimeTokenHeader, value)
			}
			principal, err := (&Authenticator{Store: store}).Authenticate(t.Context(), headers, nil)
			if !test.valid {
				require.Error(t, err)
				require.Nil(t, principal)
				return
			}
			require.NoError(t, err)
			require.Equal(t, binding, principal.(runtimeSession).binding)
		})
	}
}

func TestRuntimeCapabilityRejectsCallerIdentityAndControlBearer(t *testing.T) {
	token := strings.Repeat("ab", 32)
	digest := sha256.Sum256([]byte(token))
	for _, header := range []string{"authorization", "x-user-id", "x-agent-name", "x-kagent-insecure-runtime-identity"} {
		t.Run(header, func(t *testing.T) {
			store := &authBindings{binding: database.RuntimeGeneration{ID: uuid.New(), SessionID: uuid.New(), ActorUID: "uid", Phase: "active", TokenDigest: digest[:]}}
			headers := http.Header{}
			headers.Set(egress.RuntimeTokenHeader, token)
			headers.Set(header, "forged")
			_, err := (&Authenticator{Store: store}).Authenticate(t.Context(), headers, nil)
			require.Error(t, err)
			require.Zero(t, store.reads)
		})
	}
}
