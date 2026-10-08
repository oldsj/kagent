package payload

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Launch validates the immutable payload and durable directories, then replaces
// the launcher with the harness. CLI paths are compiled absolute in its config.
// system is the actor's filesystem root ("/"); see prepareFilesystem.
func Launch(root, system, data, platform string, check bool, environment []string, replace func(string, []string, []string) error) error {
	selected := ""
	selectedPresent := false
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		if name == PlatformEnvironment {
			if selectedPresent {
				return fmt.Errorf("duplicate selected runtime platform")
			}
			selected = value
			selectedPresent = true
		}
	}
	if selected != "linux/amd64" && selected != "linux/arm64" {
		return fmt.Errorf("%s must specify the selected runtime platform", PlatformEnvironment)
	}
	if selected != platform {
		return fmt.Errorf("selected runtime platform %s does not match running platform %s", selected, platform)
	}
	manifest, err := ValidateManifest(root, selected)
	if err != nil {
		return err
	}
	// Reject durable symlinks: neither a restored checkout nor native state can
	// redirect runtime-owned directories onto the payload or identity projection.
	for _, name := range append([]string{""}, runtimeDirectories...) {
		path := filepath.Join(data, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			return fmt.Errorf("create writable runtime directory %s: %w", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime directory %s must be a real directory", path)
		}
	}
	probe, err := os.CreateTemp(data, ".runtime-write-check-")
	if err != nil {
		return fmt.Errorf("runtime data directory %s is not writable: %w", data, err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close runtime write check: %w", err)
	}
	if err := os.Remove(probeName); err != nil {
		return fmt.Errorf("remove runtime write check: %w", err)
	}
	owned := map[string]string{
		PlatformEnvironment: selected,
		"HOME":              filepath.Join(data, "home"), "CLAUDE_CONFIG_DIR": filepath.Join(data, "claude"), "CODEX_HOME": filepath.Join(data, "codex"),
		"XDG_CONFIG_HOME": filepath.Join(data, "home", ".config"), "XDG_CACHE_HOME": filepath.Join(data, "cache"), "XDG_DATA_HOME": filepath.Join(data, "home", ".local", "share"),
		"TMPDIR": filepath.Join(data, "tmp"), "PATH": root + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"GIT_EXEC_PATH": root + "/libexec/git-core", "GIT_TEMPLATE_DIR": root + "/share/git-core/templates", "USE_BUILTIN_RIPGREP": "0", "DISABLE_UPDATES": "1",
	}
	if manifest.Provider == "claude" {
		owned["IS_SANDBOX"] = "1"
	}
	cleaned := make([]string, 0, len(environment)+len(owned))
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		if _, reserved := owned[name]; !reserved && !strings.HasPrefix(name, "LD_") && name != "BASH_ENV" && name != "ENV" {
			cleaned = append(cleaned, variable)
		}
	}
	for name, value := range owned {
		cleaned = append(cleaned, name+"="+value)
	}
	if err := os.Chdir(filepath.Join(data, "workspace")); err != nil {
		return fmt.Errorf("enter runtime workspace: %w", err)
	}
	if check {
		command := exec.Command(filepath.Join(root, "bin", manifest.Provider), "--version")
		command.Env = cleaned
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("runtime CLI linkage check: %w: %s", err, output)
		}
		expected := manifest.CLIVersion
		if !strings.Contains(string(output), expected) {
			return fmt.Errorf("runtime CLI version mismatch: %s", output)
		}
		fmt.Print(string(output))
		return nil
	}
	var configJSON string
	for _, entry := range cleaned {
		if name, value, _ := strings.Cut(entry, "="); name == "KAGENT_CONFIG_JSON" {
			configJSON = value
		}
	}
	var config struct {
		ClaudeExecutable      string `json:"claude_executable"`
		CodexExecutable       string `json:"codex_executable"`
		ExpectedClaudeVersion string `json:"expected_claude_version"`
		ExpectedCodexVersion  string `json:"expected_codex_version"`
	}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return fmt.Errorf("decode launch configuration: %w", err)
	}
	executable, version := config.ClaudeExecutable, config.ExpectedClaudeVersion
	if manifest.Provider == "codex" {
		executable, version = config.CodexExecutable, config.ExpectedCodexVersion
	}
	if executable != filepath.Join(root, "bin", manifest.Provider) || version != manifest.CLIVersion {
		return fmt.Errorf("launch configuration must select the absolute bundled CLI and locked version")
	}
	for _, warning := range prepareFilesystem(system, data, owner{uid: os.Geteuid(), gid: os.Getegid(), lchown: os.Lchown}) {
		fmt.Fprintln(os.Stderr, "runtime launch: warning:", warning)
	}
	harness := filepath.Join(root, "bin", "kagent-"+manifest.Provider)
	return replace(harness, []string{harness}, cleaned)
}
