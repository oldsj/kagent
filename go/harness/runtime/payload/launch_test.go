package payload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	claudeexecutor "github.com/kagent-dev/kagent/go/harness/claude/executor"
)

func TestLaunchExecsHarness(t *testing.T) {
	t.Run("Given a valid Claude payload, when the launcher starts, assert that it execs kagent-claude in /data with owned environment and prepared directories", func(t *testing.T) {
		f := newLaunchFixture(t, "claude")

		f.WhenLaunched(f.Environment("HOME=/evil", "LD_PRELOAD=/evil", "IS_SANDBOX=0", "BASH_ENV=/evil", "KEEP=me"))

		f.AssertThatHarnessExecuted("kagent-claude")
		require.Contains(t, f.env, "IS_SANDBOX=1")
		require.Contains(t, f.env, "HOME="+filepath.Join(f.data, "home"))
		require.Contains(t, f.env, "CLAUDE_CONFIG_DIR="+filepath.Join(f.data, "claude"))
		require.Contains(t, f.env, "USE_BUILTIN_RIPGREP=0")
		require.Contains(t, f.env, "KEEP=me")
		for _, entry := range f.env {
			require.False(t, strings.HasPrefix(entry, "LD_"), entry)
			require.False(t, strings.HasPrefix(entry, "BASH_ENV="), entry)
		}
		data, err := filepath.EvalSymlinks(f.data)
		require.NoError(t, err)
		require.Equal(t, data, f.cwd)
		require.Equal(t, os.FileMode(0711), f.dataMode, "filesystem preparation precedes exec")
		for _, name := range runtimeDirectories {
			info, err := os.Lstat(filepath.Join(f.data, name))
			require.NoError(t, err, name)
			require.True(t, info.IsDir(), name)
		}
	})

	t.Run("Given a valid Codex payload, when the launcher starts, assert that it execs kagent-codex with CODEX_HOME and without the Claude sandbox flag", func(t *testing.T) {
		f := newLaunchFixture(t, "codex")

		f.WhenLaunched(f.Environment())

		f.AssertThatHarnessExecuted("kagent-codex")
		require.Contains(t, f.env, "CODEX_HOME="+filepath.Join(f.data, "codex"))
		require.False(t, slices.ContainsFunc(f.env, func(entry string) bool { return strings.HasPrefix(entry, "IS_SANDBOX=") }))
	})
}

func TestLaunchOwnedEnvironmentOverridesInherited(t *testing.T) {
	t.Run("Given inherited values for every launcher-owned name, when the launcher starts, assert that each owned name appears once with the launcher value", func(t *testing.T) {
		f := newLaunchFixture(t, "claude")
		owned := launchEnvironment(f.root, f.data, f.platform, "claude", "", nil)
		inherited := f.Environment()
		for _, entry := range owned {
			name, _, _ := strings.Cut(entry, "=")
			if name != PlatformEnvironment {
				inherited = append(inherited, name+"=/inherited")
			}
		}

		f.WhenLaunched(inherited)

		f.AssertThatHarnessExecuted("kagent-claude")
		for _, entry := range owned {
			name, _, _ := strings.Cut(entry, "=")
			var values []string
			for _, got := range f.env {
				if value, ok := strings.CutPrefix(got, name+"="); ok {
					values = append(values, value)
				}
			}
			require.Len(t, values, 1, name)
			require.NotEqual(t, "/inherited", values[0], name)
		}
	})
}

func TestLaunchKeepsImagePath(t *testing.T) {
	t.Run("Given a development image PATH, when the launcher starts, assert that the payload bin precedes the image entries and the system directories follow", func(t *testing.T) {
		f := newLaunchFixture(t, "claude")

		f.WhenLaunched(f.Environment("PATH=/usr/local/go/bin:/usr/bin"))

		f.AssertThatHarnessExecuted("kagent-claude")
		var paths []string
		for _, entry := range f.env {
			if value, ok := strings.CutPrefix(entry, "PATH="); ok {
				paths = append(paths, value)
			}
		}
		require.Equal(t, []string{f.root + "/bin:/usr/local/go/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"}, paths)
	})
}

func TestLaunchClaudeDriver(t *testing.T) {
	scenarios := []struct {
		name     string
		provider string
		driver   string
		wantErr  string
	}{
		{name: "Given a Claude payload, when the launcher starts with an unknown driver, assert that it refuses before touching durable state", provider: "claude", driver: "other", wantErr: `KAGENT_CLAUDE_DRIVER="other" is not supported`},
		{name: "Given a Claude payload, when the launcher starts with an empty driver, assert that it execs the harness", provider: "claude", driver: ""},
		{name: "Given a Claude payload, when the launcher starts with the process driver, assert that it execs the harness", provider: "claude", driver: "process"},
		{name: "Given a Claude payload with the SDK bridge, when the launcher starts with the sdk driver, assert that it execs the harness", provider: "claude", driver: "sdk"},
		{name: "Given a Codex payload, when the launcher starts with the Claude sdk driver, assert that the unrelated selector is ignored", provider: "codex", driver: "sdk"},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			f := newLaunchFixture(t, scenario.provider)
			f.data = filepath.Join(f.data, "not-created")

			f.WhenLaunched(f.Environment(claudeDriverEnvironment + "=" + scenario.driver))

			if scenario.wantErr != "" {
				f.AssertThatLaunchFailedBeforeDurableState(scenario.wantErr)
				return
			}
			f.AssertThatHarnessExecuted("kagent-" + scenario.provider)
		})
	}

	t.Run("Given the Claude harness driver selector, when the launcher checks it, assert that both name the same variable", func(t *testing.T) {
		require.Equal(t, claudeexecutor.DriverEnvName, claudeDriverEnvironment)
	})
}

func TestLaunchRejectsInvalidPayloadOrData(t *testing.T) {
	scenarios := []struct {
		name    string
		given   func(t *testing.T, f *launchFixture)
		wantErr string
	}{
		{name: "Given a manifest with another schema, when the launcher starts, assert that it refuses", wantErr: "incompatible runtime manifest", given: func(t *testing.T, f *launchFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "manifest.json"), []byte(`{"schema":99}`), 0644))
		}},
		{name: "Given a payload binary whose bytes differ from the manifest, when the launcher starts, assert that it refuses", wantErr: "runtime checksum mismatch: bin/claude", given: func(t *testing.T, f *launchFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "bin", "claude"), []byte("wrong bytes"), 0755))
		}},
		{name: "Given a Claude manifest without the SDK bridge, when the launcher starts, assert that it refuses", wantErr: "runtime manifest is missing libexec/sdk-runner/runner.mjs", given: func(t *testing.T, f *launchFixture) {
			f.rewriteManifest(t, func(m *Manifest) { delete(m.Files, "libexec/sdk-runner/runner.mjs") })
		}},
		{name: "Given a manifest CLI version outside the release lock, when the launcher starts, assert that it refuses", wantErr: "does not match the CLI release lock", given: func(t *testing.T, f *launchFixture) {
			f.rewriteManifest(t, func(m *Manifest) { m.CLIVersion = "0.0.1" })
		}},
		{name: "Given a data path that is a file, when the launcher starts, assert that it refuses", wantErr: "create writable runtime directory", given: func(t *testing.T, f *launchFixture) {
			f.data = filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(f.data, []byte("not a directory"), 0644))
		}},
		{name: "Given a read-only data directory, when the launcher starts, assert that it refuses", wantErr: "create writable runtime directory", given: func(t *testing.T, f *launchFixture) {
			if os.Geteuid() == 0 {
				t.Skip("permission bits do not restrict host root; container smoke covers a read-only /data mount")
			}
			require.NoError(t, os.Chmod(f.data, 0500))
			t.Cleanup(func() { require.NoError(t, os.Chmod(f.data, 0700)) })
		}},
		{name: "Given a symlinked adapter directory, when the launcher starts, assert that it refuses", wantErr: "must be a real directory", given: func(t *testing.T, f *launchFixture) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(f.data, "adapter")))
		}},
		{name: "Given a symlinked bootstrap directory, when the launcher starts, assert that it refuses", wantErr: "must be a real directory", given: func(t *testing.T, f *launchFixture) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(f.data, "kagent")))
		}},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			f := newLaunchFixture(t, "claude")
			scenario.given(t, f)

			f.WhenLaunched(f.Environment())

			require.Error(t, f.err)
			require.ErrorContains(t, f.err, scenario.wantErr)
			require.False(t, f.executed)
		})
	}
}

func TestLaunchSelectedPlatform(t *testing.T) {
	actual := linuxPlatform()
	other := "linux/arm64"
	if actual == other {
		other = "linux/amd64"
	}
	scenarios := []struct {
		name        string
		environment func(f *launchFixture) []string
		given       func(t *testing.T, f *launchFixture)
		wantErr     string
	}{
		{
			// A multi-arch R resolves to this host's matching manifest. It must
			// nevertheless fail when the trusted selection requests another child.
			name:        "when the selection names another platform than the running one, assert that it refuses",
			environment: func(*launchFixture) []string { return []string{PlatformEnvironment + "=" + other} },
			wantErr:     "does not match running platform",
		},
		{
			name:        "when the manifest names another platform, assert that it refuses",
			environment: func(f *launchFixture) []string { return f.Environment() },
			given: func(t *testing.T, f *launchFixture) {
				f.rewriteManifest(t, func(m *Manifest) { m.Platform = other })
			},
			wantErr: "incompatible runtime manifest",
		},
		{name: "when no platform is selected, assert that it refuses", environment: func(*launchFixture) []string { return nil }, wantErr: "selected runtime platform"},
		{name: "when an unsupported platform is selected, assert that it refuses", environment: func(*launchFixture) []string { return []string{PlatformEnvironment + "=linux/mips"} }, wantErr: "selected runtime platform"},
		{name: "when the platform is selected twice, assert that it refuses", environment: func(f *launchFixture) []string { return f.Environment(PlatformEnvironment + "=" + other) }, wantErr: "duplicate"},
		{name: "when an empty selection precedes a valid one, assert that it refuses", environment: func(*launchFixture) []string {
			return []string{PlatformEnvironment + "=", PlatformEnvironment + "=" + actual}
		}, wantErr: "duplicate"},
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, check := range []bool{false, true} {
			mode := "launch"
			if check {
				mode = "check"
			}
			for _, scenario := range scenarios {
				t.Run("Given a "+provider+" payload in "+mode+" mode, "+scenario.name+" before touching durable state", func(t *testing.T) {
					f := newLaunchFixture(t, provider)
					f.data = filepath.Join(f.data, "not-created")
					f.check = check
					if scenario.given != nil {
						scenario.given(t, f)
					}

					f.WhenLaunched(scenario.environment(f))

					f.AssertThatLaunchFailedBeforeDurableState(scenario.wantErr)
				})
			}
		}
	}
}

func TestRuntimePath(t *testing.T) {
	const system = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, test := range []struct{ name, image, want string }{
		{"Given no image PATH, assert that the payload and system directories are used", "", "/payload/bin:" + system},
		{"Given image toolchain directories, assert that they survive in order", "/usr/local/go/bin:/go/bin:/usr/local/bin:/usr/bin", "/payload/bin:/usr/local/go/bin:/go/bin:/usr/local/bin:/usr/bin:/usr/local/sbin:/usr/sbin:/sbin:/bin"},
		{"Given relative and empty image entries, assert that they are dropped", ":bin:./node_modules/.bin:/opt/tool/bin:", "/payload/bin:/opt/tool/bin:" + system},
		{"Given an image PATH that lists the payload directory, assert that it is not duplicated", "/payload/bin:/opt/tool/bin", "/payload/bin:/opt/tool/bin:" + system},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, runtimePath("/payload", test.image))
		})
	}
}

// launchFixture is a payload with a valid manifest, a durable data directory
// and a recording exec.
type launchFixture struct {
	t        *testing.T
	root     string
	data     string
	platform string
	check    bool

	err      error
	executed bool
	path     string
	args     []string
	env      []string
	cwd      string
	dataMode os.FileMode
}

func newLaunchFixture(t *testing.T, provider string) *launchFixture {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0755))
	for _, name := range []string{"launch", "kagent-" + provider, provider, "git", "bash", "rg", "kagent-credential-refresh"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "bin", name), []byte("fixture "+name), 0755))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "libexec"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "libexec", provider), []byte("fixture cli"), 0755))
	if provider == "claude" {
		for _, name := range claudeFiles[1:] {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("fixture "+name), 0755))
		}
	}
	manifest, err := BuildManifest(root, provider)
	require.NoError(t, err)
	// Payloads are Linux-only; pin the running platform so the tests also run
	// on developer hosts. BuildManifest records the host OS.
	platform := linuxPlatform()
	manifest.Platform = platform
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "manifest.json"), data, 0644))

	original, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(original)) })
	return &launchFixture{t: t, root: root, data: t.TempDir(), platform: platform}
}

// linuxPlatform is the payload platform for the host architecture.
func linuxPlatform() string {
	return "linux/" + runtime.GOARCH
}

// Environment returns a valid platform selection followed by extra entries.
func (f *launchFixture) Environment(extra ...string) []string {
	return append([]string{PlatformEnvironment + "=" + f.platform}, extra...)
}

func (f *launchFixture) rewriteManifest(t *testing.T, change func(*Manifest)) {
	t.Helper()
	path := filepath.Join(f.root, "manifest.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var manifest Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	change(&manifest)
	raw, err = json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0644))
}

func (f *launchFixture) WhenLaunched(environment []string) {
	f.t.Helper()
	f.err = Launch(f.root, filepath.Dir(f.data), f.data, f.platform, f.check, environment, func(path string, args, env []string) error {
		f.executed, f.path, f.args, f.env = true, path, args, env
		cwd, err := os.Getwd()
		require.NoError(f.t, err)
		f.cwd = cwd
		info, err := os.Stat(f.data)
		require.NoError(f.t, err)
		f.dataMode = info.Mode().Perm()
		return nil
	})
}

func (f *launchFixture) AssertThatHarnessExecuted(binary string) {
	f.t.Helper()
	require.NoError(f.t, f.err)
	require.True(f.t, f.executed)
	require.Equal(f.t, filepath.Join(f.root, "bin", binary), f.path)
	require.Equal(f.t, []string{f.path}, f.args)
}

func (f *launchFixture) AssertThatLaunchFailedBeforeDurableState(wantErr string) {
	f.t.Helper()
	require.ErrorContains(f.t, f.err, wantErr)
	require.False(f.t, f.executed)
	_, err := os.Stat(f.data)
	require.True(f.t, os.IsNotExist(err), "rejection must precede durable state mutation")
}
