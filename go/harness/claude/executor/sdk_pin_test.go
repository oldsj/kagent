package executor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/claude/config"
)

// The bridge drives the image's pinned claude, so its SDK must be the release
// built for that CLI: SDK 0.3.N ships with Claude Code 2.1.N.
func TestSDKRunnerPinMatchesClaudeVersion(t *testing.T) {
	raw, err := os.ReadFile("../sdk-runner/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	sdk := manifest.Dependencies["@anthropic-ai/claude-agent-sdk"]
	cli := config.PinnedClaudeVersion
	if sdk[strings.LastIndex(sdk, ".")+1:] != cli[strings.LastIndex(cli, ".")+1:] || !strings.HasPrefix(sdk, "0.3.") || !strings.HasPrefix(cli, "2.1.") {
		t.Fatalf("SDK %s does not match pinned Claude %s; update sdk-runner/package.json and its lockfile", sdk, cli)
	}
}
