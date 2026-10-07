package payload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixturePayload(t *testing.T, provider string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0755))
	for _, name := range []string{"launch", "kagent-" + provider, provider, "git", "bash", "rg", "kagent-credential-refresh"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "bin", name), []byte("fixture"), 0755))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "libexec"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "libexec", provider), []byte("fixture"), 0755))
	manifest, err := BuildManifest(root, provider)
	require.NoError(t, err)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "manifest.json"), data, 0644))
	return root
}

func TestLauncherValidationAndExec(t *testing.T) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	for _, name := range []string{"happy", "bad platform", "bad manifest", "corrupt binary", "unwritable data", "read-only data", "state symlink", "relative CLI"} {
		t.Run(name, func(t *testing.T) {
			root := fixturePayload(t, "claude")
			data := t.TempDir()
			original, err := os.Getwd()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.Chdir(original)) })
			switch name {
			case "bad platform":
				platform = "linux/unsupported"
			case "bad manifest":
				require.NoError(t, os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"schema":99}`), 0644))
			case "corrupt binary":
				require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "claude"), []byte("wrong bytes"), 0755))
			case "unwritable data":
				data = filepath.Join(t.TempDir(), "file")
				require.NoError(t, os.WriteFile(data, []byte("not a directory"), 0644))
			case "read-only data":
				if os.Geteuid() == 0 {
					t.Skip("permission bits do not restrict host root; container smoke covers a read-only /data mount")
				}
				require.NoError(t, os.Chmod(data, 0500))
				t.Cleanup(func() { require.NoError(t, os.Chmod(data, 0700)) })
			case "state symlink":
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(data, "adapter")))
			}
			executed := false
			configPath := root + "/bin/claude"
			if name == "relative CLI" {
				configPath = "claude"
			}
			err = Launch(root, data, platform, false, []string{PlatformEnvironment + "=" + runtime.GOOS + "/" + runtime.GOARCH, "HOME=/evil", "LD_PRELOAD=/evil", "IS_SANDBOX=0", "KAGENT_CONFIG_JSON=" + `{"claude_executable":"` + configPath + `","expected_claude_version":"` + LockedRelease("claude").Version + `"}`}, func(path string, args, env []string) error {
				executed = true
				require.Equal(t, filepath.Join(root, "bin", "kagent-claude"), path)
				require.Equal(t, []string{path}, args)
				require.Contains(t, env, "IS_SANDBOX=1")
				require.Contains(t, env, "HOME="+filepath.Join(data, "home"))
				require.Contains(t, env, "CLAUDE_CONFIG_DIR="+filepath.Join(data, "claude"))
				for _, entry := range env {
					require.False(t, strings.HasPrefix(entry, "LD_"))
				}
				cwd, err := os.Getwd()
				require.NoError(t, err)
				require.Equal(t, filepath.Join(data, "workspace"), cwd)
				return nil
			})
			if name == "happy" {
				require.NoError(t, err)
				require.True(t, executed)
			} else {
				require.Error(t, err)
				require.False(t, executed)
			}
			platform = runtime.GOOS + "/" + runtime.GOARCH
		})
	}
}

func TestCodexLauncherExec(t *testing.T) {
	root := fixturePayload(t, "codex")
	data := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(cwd)) })
	config := `{"codex_executable":"` + root + `/bin/codex","expected_codex_version":"` + LockedRelease("codex").Version + `"}`
	executed := false
	err = Launch(root, data, runtime.GOOS+"/"+runtime.GOARCH, false, []string{PlatformEnvironment + "=" + runtime.GOOS + "/" + runtime.GOARCH, "KAGENT_CONFIG_JSON=" + config}, func(path string, args, env []string) error {
		executed = true
		require.Equal(t, root+"/bin/kagent-codex", path)
		require.Contains(t, env, "CODEX_HOME="+data+"/codex")
		return nil
	})
	require.NoError(t, err)
	require.True(t, executed)
}

func TestLauncherSelectedPlatform(t *testing.T) {
	actual := runtime.GOOS + "/" + runtime.GOARCH
	other := "linux/arm64"
	if actual == other {
		other = "linux/amd64"
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, name := range []string{"index resolves actual child", "manifest differs", "missing", "invalid", "duplicate", "empty duplicate"} {
			for _, check := range []bool{false, true} {
				t.Run(provider+"/"+name+fmt.Sprint(check), func(t *testing.T) {
					root := fixturePayload(t, provider)
					data := filepath.Join(t.TempDir(), "not-created")
					environment := []string{PlatformEnvironment + "=" + actual}
					expected := "selected runtime platform"
					switch name {
					case "index resolves actual child":
						// A multi-arch R resolves to this host's matching manifest. It must
						// nevertheless fail when the trusted selection requests another child.
						environment = []string{PlatformEnvironment + "=" + other}
						expected = "does not match running platform"
					case "manifest differs":
						path := filepath.Join(root, "manifest.json")
						raw, err := os.ReadFile(path)
						require.NoError(t, err)
						var manifest Manifest
						require.NoError(t, json.Unmarshal(raw, &manifest))
						manifest.Platform = other
						raw, err = json.Marshal(manifest)
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(path, raw, 0644))
						expected = "incompatible runtime manifest"
					case "missing":
						environment = nil
					case "invalid":
						environment = []string{PlatformEnvironment + "=linux/mips"}
					case "duplicate":
						environment = append(environment, PlatformEnvironment+"="+other)
						expected = "duplicate"
					case "empty duplicate":
						environment = []string{PlatformEnvironment + "=", PlatformEnvironment + "=" + actual}
						expected = "duplicate"
					}
					executed := false
					err := Launch(root, data, actual, check, environment, func(string, []string, []string) error { executed = true; return nil })
					require.ErrorContains(t, err, expected)
					require.False(t, executed)
					_, err = os.Stat(data)
					require.True(t, os.IsNotExist(err), "platform rejection must precede durable state mutation")
				})
			}
		}
	}
}
