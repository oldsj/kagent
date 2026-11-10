package translator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexconfig "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

var cliVersionPattern = regexp.MustCompile(`^[0-9]+[.][0-9]+[.][0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

var digestImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

// Composition is the operator-resolved, immutable pairing of a development
// image (D) with a runtime payload image (R). A nil composition keeps the
// single-image revision and its digest unchanged.
type Composition struct {
	DevelopmentImage string      `json:"developmentImage"`
	PayloadImage     string      `json:"payloadImage"`
	CLIVersion       string      `json:"cliVersion"`
	Platform         string      `json:"platform"`
	PolicyIdentity   string      `json:"policyIdentity"`
	Provider         HarnessType `json:"provider"`
	Schema           uint32      `json:"schema"`
}

// composedCapabilities extend Substrate's default actor set (AUDIT_WRITE, KILL,
// NET_BIND_SERVICE). Substrate runs the process as uid 0; development-image
// tooling that drops to the image user (runuser, chown of its scratch
// directories, root cleanup afterwards) needs these file and identity
// capabilities. No network or mount capability is granted: egress stays
// enforced outside the sandbox.
var composedCapabilities = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}

// ContainerCapabilities returns the capabilities a composed runtime container
// adds to Substrate's default set, in the order the ActorTemplate records them.
func (Composition) ContainerCapabilities() []string {
	return slices.Clone(composedCapabilities)
}

// Validate rejects mutable references and unsupported payload contracts.
func (c Composition) Validate() error {
	for _, ref := range []string{c.DevelopmentImage, c.PayloadImage} {
		if len(ref) > 512 || !digestImagePattern.MatchString(ref) {
			return fmt.Errorf("composition requires a sha256 digest-pinned image: %q", ref)
		}
	}
	if !SupportedPlatform(c.Platform) {
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

// SupportedPlatform reports whether a composed runtime can target platform.
func SupportedPlatform(platform string) bool {
	return platform == "linux/amd64" || platform == "linux/arm64"
}

// WorkerPoolPlatform derives the platform a WorkerPool is pinned to from its
// node selector. It returns "" when the pool does not pin a single Linux
// architecture, which composition rejects.
func WorkerPoolPlatform(nodeSelector map[string]string) string {
	arch := nodeSelector["kubernetes.io/arch"]
	if os, ok := nodeSelector["kubernetes.io/os"]; ok && os != "linux" {
		return ""
	}
	platform := "linux/" + arch
	if !SupportedPlatform(platform) {
		return ""
	}
	return platform
}

// ComposeRevision derives the composed variant of a compiled base revision.
// The development image cannot supply command, mounts or environment through
// this typed selection; the payload's launcher is always the entrypoint.
func ComposeRevision(base Revision, selection Composition) (Revision, error) {
	if err := selection.Validate(); err != nil {
		return Revision{}, err
	}
	if base.Composition != nil {
		return Revision{}, fmt.Errorf("revision is already composed")
	}
	if base.Harness != selection.Provider {
		return Revision{}, fmt.Errorf("composition provider %q differs from the compiled %q harness", selection.Provider, base.Harness)
	}
	if base.WorkerPoolPlatform != selection.Platform {
		return Revision{}, fmt.Errorf("WorkerPool %s must pin kubernetes.io/arch to the selected platform %s", base.WorkerPoolName, selection.Platform)
	}
	if err := checkComposedEnvironment(base.Environment); err != nil {
		return Revision{}, err
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
	composed := base
	composed.Composition = &selection
	composed.Image = selection.DevelopmentImage
	composed.Command = []string{payload.Root + "/bin/launch"}
	composed.ConfigJSON = configJSON
	composed.Environment = append(slices.Clone(base.Environment), corev1.EnvVar{Name: payload.PlatformEnvironment, Value: selection.Platform})
	return composed, nil
}

// checkComposedEnvironment rejects Harness environment that would redirect the
// payload's loader, homes or runtime paths, or select a driver the payload
// does not ship.
func checkComposedEnvironment(environment []corev1.EnvVar) error {
	for _, variable := range environment {
		if variable.ValueFrom != nil || reservedCompositionEnvironment(variable.Name) {
			return fmt.Errorf("environment %q conflicts with the composed runtime", variable.Name)
		}
		value := variable.Value
		if strings.Contains(value, payload.Root) || strings.Contains(value, "/run/kagent/") || strings.Contains(value, "/data/") || value == "/data" || value == "/run/kagent" {
			return fmt.Errorf("environment %q references reserved runtime paths", variable.Name)
		}
		if variable.Name == "KAGENT_CLAUDE_DRIVER" && value != "" && value != "process" {
			return fmt.Errorf("composed Claude runtimes support only the process driver")
		}
	}
	return nil
}

func reservedCompositionEnvironment(name string) bool {
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "MAINLOOP_RUNTIME_") {
		return true
	}
	switch name {
	case "USE_BUILTIN_RIPGREP", "HOME", "PATH", "BASH_ENV", "ENV", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "IS_SANDBOX", "DISABLE_UPDATES":
		return true
	}
	return false
}
