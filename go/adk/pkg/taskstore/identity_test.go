package taskstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestGenerationRoutingHintRejectsLegacyAndMalformedNames(t *testing.T) {
	id := uuid.NewString()
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"session-" + id + "-0123456789abcdef", true},
		{"session-" + id, false}, {"session-" + id + "-0123456789abcdeg", false},
		{"session-" + id + "-0123456789ABCDEF", false}, {"session-invalid-0123456789abcdef", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "name")
			require.NoError(t, os.WriteFile(path, []byte(test.name), 0600))
			got, err := New(nil, path).sessionID()
			if test.valid {
				require.NoError(t, err)
				require.Equal(t, id, got)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCallbackContextRequiresNonsecretGatewayPlaceholder(t *testing.T) {
	controller, err := controllerclient.New(controllerclient.Config{APIURL: "http://controller.invalid"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, controller.Close()) })
	// No projection fields or token files are read by the callback contract.
	store := New(controller, filepath.Join(t.TempDir(), "missing-name"))
	parent := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("authorization", "Bearer control", "x-kagent-runtime-token", "forged", "x-kagent-insecure-runtime-identity", "forged"))
	ctx, cancel, err := store.callContext(parent)
	require.NoError(t, err)
	defer cancel()
	md, _ := metadata.FromOutgoingContext(ctx)
	require.Equal(t, []string{RuntimeTokenPlaceholder}, md.Get(RuntimeTokenHeader))
	require.Empty(t, md.Get("authorization"))
	require.Empty(t, md.Get("x-kagent-insecure-runtime-identity"))
}
