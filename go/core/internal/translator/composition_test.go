package translator

import (
	"encoding/json"
	"strings"
	"testing"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestLegacyRevisionHashUnchangedByComposition(t *testing.T) {
	revision := Revision{Namespace: "agents", AgentName: "helper"}
	id, err := revision.Digest()
	require.NoError(t, err)
	// Calculated with the pre-composition Revision.Digest serialization.
	require.Equal(t, "24c9e0bdae9b899e9fdaa342c6d6d5ddf28b900a62aca356ccb4169edddadd4e", id.String())
	revision.NativeProvider = HarnessTypeClaude
	unchanged, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, id, unchanged)
}

func TestComposeRevision(t *testing.T) {
	for _, provider := range []HarnessType{HarnessTypeClaude, HarnessTypeCodex} {
		t.Run(string(provider), func(t *testing.T) {
			var config []byte
			var err error
			if provider == HarnessTypeClaude {
				config, err = json.Marshal(claudeconfig.Production("model", "help"))
			} else {
				cfg := codexconfig.Production("model", "help")
				cfg.Provider = codexconfig.Provider{Name: "openai", BaseURL: "https://api.openai.com/v1"}
				config, err = json.Marshal(cfg)
			}
			require.NoError(t, err)
			base := Revision{Image: "legacy", NativeProvider: provider, ConfigJSON: config}
			selection := Composition{DevelopmentImage: "registry/dev@sha256:" + strings.Repeat("a", 64), PayloadImage: "registry/runtime@sha256:" + strings.Repeat("b", 64), Provider: provider, Platform: "linux/arm64", PolicyIdentity: "accepted-v1", Schema: payload.Schema, CLIVersion: payload.LockedRelease(string(provider)).Version}
			composed, err := ComposeRevision(base, selection)
			require.NoError(t, err)
			require.Equal(t, "legacy", base.Image)
			require.Nil(t, base.Composition)
			require.Equal(t, selection.DevelopmentImage, composed.Image)
			require.Equal(t, []string{payload.Root + "/bin/launch"}, composed.Command)
			require.Contains(t, string(composed.ConfigJSON), payload.Root+"/bin/"+string(provider))
			first, err := composed.Digest()
			require.NoError(t, err)
			for _, mutate := range []func(*Composition){
				func(c *Composition) { c.DevelopmentImage = "registry/dev@sha256:" + strings.Repeat("c", 64) },
				func(c *Composition) { c.PayloadImage = "registry/runtime@sha256:" + strings.Repeat("c", 64) },
				func(c *Composition) { c.Platform = "linux/amd64" },
				func(c *Composition) { c.PolicyIdentity = "accepted-v2" },
				func(c *Composition) { c.CLIVersion = "9.9.9" },
			} {
				changed := selection
				mutate(&changed)
				next, err := ComposeRevision(base, changed)
				require.NoError(t, err)
				digest, err := next.Digest()
				require.NoError(t, err)
				require.NotEqual(t, first, digest)
			}
			for _, name := range []string{"HOME", "PATH", "LD_PRELOAD", "MAINLOOP_RUNTIME_ROOT", payload.PlatformEnvironment, "KAGENT_CONFIG_JSON", "BASH_ENV", "CODEX_HOME", "TMPDIR"} {
				input := base
				input.Environment = []corev1.EnvVar{{Name: name, Value: "evil"}}
				_, err := ComposeRevision(input, selection)
				require.ErrorContains(t, err, "reserved")
			}
			for _, path := range []string{payload.Root + "/bin/launch", "/run/kagent/identity", "/data/adapter"} {
				input := base
				input.Environment = []corev1.EnvVar{{Name: "CUSTOM", Value: path}}
				_, err := ComposeRevision(input, selection)
				require.ErrorContains(t, err, "reserved")
			}
			for _, mutate := range []func(*Composition){
				func(c *Composition) { c.DevelopmentImage = "registry/dev:latest" },
				func(c *Composition) { c.PayloadImage = "registry/runtime:latest" },
				func(c *Composition) { c.Schema = 99 },
			} {
				invalid := selection
				mutate(&invalid)
				_, err := ComposeRevision(base, invalid)
				require.Error(t, err)
			}
		})
	}
}
