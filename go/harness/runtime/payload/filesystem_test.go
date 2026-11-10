package payload

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func runtimeTree(t *testing.T) (string, string) {
	t.Helper()
	system := t.TempDir()
	data := filepath.Join(system, "data")
	for _, name := range runtimeDirectories {
		require.NoError(t, os.MkdirAll(filepath.Join(data, name), 0700))
	}
	return system, data
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

// recorder treats the test user as foreign so every ownership check fires.
type recorder struct {
	paths []string
	fail  string
}

func (r *recorder) owner() owner {
	return owner{uid: os.Geteuid() + 1, gid: os.Getegid() + 1, lchown: func(path string, _, _ int) error {
		if path == r.fail {
			return errors.New("operation not permitted")
		}
		r.paths = append(r.paths, path)
		return nil
	}}
}

func TestPrepareFilesystemModes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		system, data   os.FileMode
		wantSystem     os.FileMode
		wantData       os.FileMode
		privateRuntime os.FileMode
	}{
		{name: "Given the Substrate 0700 root, when the filesystem is prepared, assert that root is widened and data is traversal-only", system: 0700, data: 0700, wantSystem: 0755, wantData: 0711, privateRuntime: 0700},
		{name: "Given permissive modes, when the filesystem is prepared, assert that only data and runtime directories are narrowed", system: 0775, data: 0755, wantSystem: 0775, wantData: 0711, privateRuntime: 0755},
		{name: "Given an already prepared tree, when the filesystem is prepared, assert that modes are unchanged", system: 0755, data: 0711, wantSystem: 0755, wantData: 0711, privateRuntime: 0700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system, data := runtimeTree(t)
			require.NoError(t, os.Chmod(filepath.Join(data, "claude"), tc.privateRuntime))
			require.NoError(t, os.Chmod(data, tc.data))
			require.NoError(t, os.Chmod(system, tc.system))
			// The tree's own owner: BSD-style hosts give new directories the
			// parent's group rather than the process's effective group.
			uid, gid, err := fileOwner(data)
			require.NoError(t, err)
			identity := owner{uid: uid, gid: gid, lchown: func(string, int, int) error {
				t.Fatal("identity-owned tree must not be chowned")
				return nil
			}}
			require.Empty(t, prepareFilesystem(system, data, identity))
			require.Equal(t, tc.wantSystem, mode(t, system))
			require.Equal(t, tc.wantData, mode(t, data))
			for _, name := range runtimeDirectories {
				require.Equal(t, os.FileMode(0700), mode(t, filepath.Join(data, name)), name)
			}
		})
	}
}

func TestPrepareFilesystemFailuresAreWarnings(t *testing.T) {
	t.Run("Given a missing system root, when the filesystem is prepared, assert that the failure is a warning and later steps still run", func(t *testing.T) {
		system, data := runtimeTree(t)
		missing := filepath.Join(system, "missing")
		identity := owner{uid: os.Geteuid(), gid: os.Getegid(), lchown: os.Lchown}
		warnings := prepareFilesystem(missing, data, identity)
		require.Len(t, warnings, 1)
		require.ErrorContains(t, warnings[0], "inspect "+missing)
		require.Equal(t, os.FileMode(0711), mode(t, data), "later steps still run")
	})
}

func TestPrepareFilesystemReclaimsForeignScratch(t *testing.T) {
	t.Run("Given foreign-owned scratch under data, when the filesystem is prepared, assert that it is reclaimed without following symlinks or walking runtime directories", func(t *testing.T) {
		system, data := runtimeTree(t)
		scratch := filepath.Join(data, "test-postgres")
		require.NoError(t, os.MkdirAll(filepath.Join(scratch, "base", "1"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "base", "1", "relation"), []byte("x"), 0600))
		// The walk must chown the link itself, never descend into its target.
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0600))
		require.NoError(t, os.Symlink(outside, filepath.Join(scratch, "link")))
		require.NoError(t, os.WriteFile(filepath.Join(data, "loose-file"), []byte("x"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(data, "workspace", "checkout-file"), []byte("x"), 0600))

		recorded := &recorder{}
		require.Empty(t, prepareFilesystem(system, data, recorded.owner()))

		want := []string{
			filepath.Join(data, "loose-file"),
			scratch,
			filepath.Join(scratch, "base"),
			filepath.Join(scratch, "base", "1"),
			filepath.Join(scratch, "base", "1", "relation"),
			filepath.Join(scratch, "link"),
		}
		// Launcher-owned directories are restored themselves, not their contents.
		for _, name := range runtimeDirectories {
			want = append(want, filepath.Join(data, name))
		}
		slices.Sort(want)
		got := slices.Clone(recorded.paths)
		slices.Sort(got)
		require.Equal(t, want, got)
		require.NotContains(t, recorded.paths, filepath.Join(outside, "secret"))
		require.NotContains(t, recorded.paths, filepath.Join(data, "workspace", "checkout-file"))
	})
}

func TestPrepareFilesystemReclaimFailureContinues(t *testing.T) {
	t.Run("Given one unreclaimable scratch entry, when the filesystem is prepared, assert that it is a warning and other entries are still reclaimed", func(t *testing.T) {
		system, data := runtimeTree(t)
		for _, name := range []string{"stale-a", "stale-b"} {
			require.NoError(t, os.MkdirAll(filepath.Join(data, name, "nested"), 0700))
		}
		recorded := &recorder{fail: filepath.Join(data, "stale-a")}
		warnings := prepareFilesystem(system, data, recorded.owner())
		require.Len(t, warnings, 1)
		require.ErrorContains(t, warnings[0], "reclaim "+filepath.Join(data, "stale-a"))
		require.Contains(t, recorded.paths, filepath.Join(data, "stale-b", "nested"))
	})
}
