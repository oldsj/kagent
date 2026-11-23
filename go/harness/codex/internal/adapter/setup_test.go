package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
)

// The observe check compares config.toml byte for byte. Once the install has
// rendered the workspace trust entry, the pinned Codex leaves the file
// unchanged (TestPinnedCodexLeavesTrustedWorkspaceUnchanged), so every later
// observe, each turn's and a restart's, must pass. In 0.4 the missing entry
// made every turn after the first fail as "Workspace preparation is incomplete".
func TestSetupObservePassesOnceCodexTrustsTheWorkspace(t *testing.T) {
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	durable := filepath.Dir(workspaceDir)
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	readOrigin, pushOrigin := apiworkspace.GitInstanceOrigins("mainloop")
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin}
	cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: apiworkspace.MCPInstanceOrigin("mainloop") + "/mcp"}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	store, err := continuation.New(filepath.Join(durable, "adapter"), "codex", func(string) error { return nil })
	require.NoError(t, err)
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(raw)
	require.NoError(t, err)
	setupDigest, err := apiworkspace.SetupDigest("agent")
	require.NoError(t, err)
	assignment := workspace.Preparation{Provider: "codex", Profile: "agent", SetupDigest: setupDigest, ConfigDigest: configDigest, MCPDigest: mcpDigest}
	installer := &SetupInstaller{Input: Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable}, Store: store}

	_, _, err = installer.Setup(t.Context(), assignment, false)
	require.NoError(t, err)
	configPath := filepath.Join(durable, "codex", "config.toml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(before), string(workspaceTrustEntry(workspaceDir)))
	for range 2 {
		_, _, err = installer.Setup(t.Context(), assignment, true)
		require.NoError(t, err)
	}
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// preparedInstaller installs a prepared Codex workspace under env, as the
// first harness process does, and returns its directories and assignment.
func preparedInstaller(t *testing.T, env []string) (Input, workspace.Preparation) {
	t.Helper()
	workspaceDir := filepath.Join(t.TempDir(), "data", "workspace")
	durable := filepath.Dir(workspaceDir)
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	readOrigin, pushOrigin := apiworkspace.GitInstanceOrigins("mainloop")
	cfg.Git = &apiworkspace.Git{Origins: []string{"github.com"}, ReadProxyOrigin: &readOrigin, PushProxyOrigin: &pushOrigin}
	cfg.MCPServers = map[string]config.MCPServer{"mainloop": {URL: apiworkspace.MCPInstanceOrigin("mainloop") + "/mcp"}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	store, err := continuation.New(filepath.Join(durable, "adapter"), "codex", func(string) error { return nil })
	require.NoError(t, err)
	configDigest, mcpDigest, err := apiworkspace.ConfigDigests(raw)
	require.NoError(t, err)
	setupDigest, err := apiworkspace.SetupDigest("agent")
	require.NoError(t, err)
	assignment := workspace.Preparation{Provider: "codex", Profile: "agent", SetupDigest: setupDigest, ConfigDigest: configDigest, MCPDigest: mcpDigest}
	input := Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable, Environment: env}
	_, _, err = (&SetupInstaller{Input: input, Store: store}).Setup(t.Context(), assignment, false)
	require.NoError(t, err)
	return input, assignment
}

func renderFor(t *testing.T, input Input) []byte {
	t.Helper()
	cfg, err := config.Parse(input.ConfigJSON)
	require.NoError(t, err)
	cfg, _ = withNativeTelemetry(cfg, input)
	out, err := renderInputConfig(cfg, input)
	require.NoError(t, err)
	return out
}

// A harness process that restarts a prepared workspace (a restore after
// quiesce, or a new harness image) renders config.toml from its own code and
// environment, which may differ from the process that wrote the file. The
// restart check asks whether the file changed since a harness wrote it, not
// whether this harness would render the same bytes: otherwise every harness
// upgrade that changes the rendering fails every prepared Codex session at
// startup.
func TestRestartAcceptsConfigAnEarlierHarnessRendered(t *testing.T) {
	// Given a workspace prepared by a harness without trace export
	input, assignment := preparedInstaller(t, nil)

	// When the harness restarts with an environment that renders config.toml differently
	restarted := input
	restarted.Environment = []string{"OTEL_TRACES_EXPORTER=otlp", "OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4317"}
	require.NotEqual(t, renderFor(t, input), renderFor(t, restarted), "the test needs two different renders")

	// Then the restart check passes, and the restarted harness's own render is checked each turn
	_, _, err := (&SetupInstaller{Input: restarted, Restart: true}).Setup(t.Context(), assignment, true)
	require.NoError(t, err)
	_, err = New(t.Context(), restarted)
	require.NoError(t, err)
	_, _, err = (&SetupInstaller{Input: restarted}).Setup(t.Context(), assignment, true)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(input.DurableDir, "codex", "config.toml"))
	require.NoError(t, err)
	require.Equal(t, renderFor(t, restarted), got)
}

// The restart check still refuses a config.toml changed after a harness wrote
// it, and each turn's check still compares against the render.
func TestRestartRefusesConfigChangedSinceItWasWritten(t *testing.T) {
	input, assignment := preparedInstaller(t, nil)
	path := filepath.Join(input.DurableDir, "codex", "config.toml")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(contents, "\n[mcp_servers.extra]\nurl = \"http://example\"\n"...), 0o600))

	_, _, err = (&SetupInstaller{Input: input, Restart: true}).Setup(t.Context(), assignment, true)
	require.EqualError(t, err, "installed Codex configuration differs")
	_, _, err = (&SetupInstaller{Input: input}).Setup(t.Context(), assignment, true)
	require.EqualError(t, err, "installed Codex configuration differs")
}

// A workspace written by a harness that recorded no digest is checked against
// the render, as before, on restart as well.
func TestRestartWithoutARecordComparesTheRender(t *testing.T) {
	input, assignment := preparedInstaller(t, nil)
	require.NoError(t, os.Remove(configRecordPath(input)))

	_, _, err := (&SetupInstaller{Input: input, Restart: true}).Setup(t.Context(), assignment, true)
	require.NoError(t, err)
	restarted := input
	restarted.Environment = []string{"OTEL_TRACES_EXPORTER=otlp", "OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4317"}
	_, _, err = (&SetupInstaller{Input: restarted, Restart: true}).Setup(t.Context(), assignment, true)
	require.EqualError(t, err, "installed Codex configuration differs")
}
