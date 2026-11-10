package workspace

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Mainloop hashes the same Standing text, so these digests are pinned.
func TestStandingSetupDigests(t *testing.T) {
	for _, test := range []struct {
		profile, digest string
	}{
		{"supervisor", "c9b2c5bf815f6b0dd45ee11f02e343ab6a25d4ce39c93663e6feb25281cb87f6"},
		{"child", "7e8a64abb17aaa575ab317f07ad673defc495d97adb641283e5d1cc636b30258"},
		{"agent", "a5fb1bb1e406ff7937b9d9e2e862dff43925d4df54d4bdd9fb010d7e2825ccb3"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			digest, err := SetupDigest(test.profile)
			require.NoError(t, err)
			require.Equal(t, test.digest, digest)
		})
	}
	for _, profile := range []string{"", "owner", "Agent", "arbitrary"} {
		_, err := Standing(profile)
		require.Error(t, err)
		_, err = SetupDigest(profile)
		require.Error(t, err)
	}
}

// testGitBlock builds the compiled Git policy JSON for a Mainloop instance
// namespace, in the shape ConfigDigests parses to resolve that instance.
func testGitBlock(namespace string) string {
	read, push := GitInstanceOrigins(namespace)
	return `"git":{"origins":["github.com"],"readProxyOrigin":"` + read + `","pushProxyOrigin":"` + push + `"}`
}

// testMCPServerBlock builds one compiled MCP server entry under key,
// independent of namespace: ConfigDigests binds by URL origin, never by key.
func testMCPServerBlock(key, namespace string) string {
	return `"mcp_servers":{"` + key + `":{"url":"` + MCPInstanceOrigin(namespace) + `/mcp"}}`
}

func TestConfigDigestsBindsMCPServerToGitInstance(t *testing.T) {
	for _, test := range []struct {
		name                       string
		gitNamespace, mcpNamespace string
		wantErr                    bool
	}{
		{"default instance accepted", "mainloop", "mainloop", false},
		{"second instance accepted with its own server", "mainloop-dev", "mainloop-dev", false},
		{"second instance rejects the default instance's server", "mainloop-dev", "mainloop", true},
		{"default instance rejects the second instance's server", "mainloop", "mainloop-dev", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{` + testMCPServerBlock("mainloop", test.mcpNamespace) + `,` + testGitBlock(test.gitNamespace) + `}`)
			_, _, err := ConfigDigests(raw)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestConfigDigestsRejectsIncompleteConfig(t *testing.T) {
	for name, raw := range map[string]string{
		"no MCP server":        `{` + testGitBlock("mainloop") + `}`,
		"no Git policy":        `{` + testMCPServerBlock("mainloop", "mainloop") + `}`,
		"direct Git transport": `{` + testMCPServerBlock("mainloop", "mainloop") + `,"git":{"origins":["github.com"]}}`,
		"not an object":        `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ConfigDigests([]byte(raw))
			require.Error(t, err)
		})
	}
}

// The default instance's digests stay byte-identical to 0.4, and key order and
// whitespace in config_json do not change them.
func TestConfigDigestsDefaultInstancePinned(t *testing.T) {
	for _, raw := range []string{
		`{"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}},` + testGitBlock("mainloop") + `}`,
		"{\n  " + testGitBlock("mainloop") + ",\n  \"mcp_servers\": {\"mainloop\":{\"url\":\"http://mainloop-mcp.mainloop.svc.cluster.local/mcp\"}}\n}\n",
	} {
		config, mcp, err := ConfigDigests([]byte(raw))
		require.NoError(t, err)
		require.Equal(t, "ae54f9fb2ab9f111008b18a746489ce8c999e11f00ee9e4d9e3db7835d7f299c", config)
		require.Equal(t, "0970deaef31746ebd17de132aac54eb5dc11a43652530a9057de8e2e7c065dc4", mcp)
	}
}
