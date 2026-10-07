package translator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

var cliVersionPattern = regexp.MustCompile(`^[0-9]+[.][0-9]+[.][0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

var digestImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

// Composition is the operator-resolved immutable environment and payload pair.
// Nil composition retains the legacy image and revision serialization.
type Composition struct {
	DevelopmentImage string      `json:"developmentImage"`
	PayloadImage     string      `json:"payloadImage"`
	CLIVersion       string      `json:"cliVersion"`
	Platform         string      `json:"platform"`
	PolicyIdentity   string      `json:"policyIdentity"`
	Provider         HarnessType `json:"provider"`
	Schema           uint32      `json:"schema"`
}

// Validate rejects mutable references and unsupported payload contracts.
func (c Composition) Validate() error {
	for _, ref := range []string{c.DevelopmentImage, c.PayloadImage} {
		if len(ref) > 512 || !digestImagePattern.MatchString(ref) {
			return fmt.Errorf("composition requires a sha256 digest-pinned image: %q", ref)
		}
	}
	if c.Platform != "linux/amd64" && c.Platform != "linux/arm64" {
		return fmt.Errorf("unsupported runtime platform %q", c.Platform)
	}
	if c.Provider != HarnessTypeClaude && c.Provider != HarnessTypeCodex {
		return fmt.Errorf("unsupported composed harness %q", c.Provider)
	}
	if strings.TrimSpace(c.PolicyIdentity) == "" || len(c.PolicyIdentity) > 256 {
		return fmt.Errorf("immutable environment policy identity is required")
	}
	if len(c.CLIVersion) > 64 || !cliVersionPattern.MatchString(c.CLIVersion) {
		return fmt.Errorf("runtime catalog must supply a CLI release version")
	}
	if c.Schema != payload.Schema {
		return fmt.Errorf("unsupported runtime payload schema %d", c.Schema)
	}
	return nil
}

// ComposeRevision compiles a selected environment without changing the base revision.
// D cannot supply command, mounts or environment through this typed selection.
func ComposeRevision(base Revision, selection Composition) (Revision, error) {
	if err := selection.Validate(); err != nil {
		return Revision{}, err
	}
	if base.NativeProvider != selection.Provider {
		return Revision{}, fmt.Errorf("composition provider differs from compiled native harness")
	}
	for _, variable := range base.Environment {
		if reservedCompositionEnvironment(variable.Name) || strings.Contains(variable.Value, payload.Root) || strings.Contains(variable.Value, "/run/kagent/") || strings.Contains(variable.Value, "/data/") || variable.Value == "/data" || variable.Value == "/run/kagent" {
			return Revision{}, fmt.Errorf("environment %q conflicts with reserved runtime paths or configuration", variable.Name)
		}
	}
	if len(base.Command) != 0 || len(base.Args) != 0 {
		return Revision{}, fmt.Errorf("composed harness cannot override the runtime launch command")
	}
	var configJSON []byte
	var err error
	switch selection.Provider {
	case HarnessTypeClaude:
		config, parseErr := claudeconfig.Parse(base.ConfigJSON)
		if parseErr != nil {
			return Revision{}, fmt.Errorf("decode composed Claude config: %w", parseErr)
		}
		config.ClaudeExecutable = payload.Root + "/bin/claude"
		config.ExpectedClaudeVersion = selection.CLIVersion
		configJSON, err = json.Marshal(config)
	case HarnessTypeCodex:
		config, parseErr := codexconfig.Parse(base.ConfigJSON)
		if parseErr != nil {
			return Revision{}, fmt.Errorf("decode composed Codex config: %w", parseErr)
		}
		config.CodexExecutable = payload.Root + "/bin/codex"
		config.ExpectedCodexVersion = selection.CLIVersion
		configJSON, err = json.Marshal(config)
	}
	if err != nil {
		return Revision{}, fmt.Errorf("encode composed runtime config: %w", err)
	}
	base.Composition = &selection
	base.Image = selection.DevelopmentImage
	base.Command = []string{payload.Root + "/bin/launch"}
	base.ConfigJSON = configJSON
	return base, nil
}

func reservedCompositionEnvironment(name string) bool {
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "MAINLOOP_RUNTIME_") {
		return true
	}
	switch name {
	case "USE_BUILTIN_RIPGREP", "HOME", "PATH", "BASH_ENV", "ENV", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "KAGENT_CONFIG_JSON", "KAGENT_AGENT_CARD_JSON", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME":
		return true
	}
	return false
}
