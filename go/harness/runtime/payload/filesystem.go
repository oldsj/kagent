package payload

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// runtimeDirectories are the launcher-owned durable directories. They stay
// private to the launcher's identity so image-user processes (for example a
// test database started through runuser) cannot read provider state or
// credentials.
var runtimeDirectories = []string{"workspace", "adapter", "generated", "home", "claude", "codex", "cache", "tmp"}

// owner is the identity durable state is restored to, normally root. Tests
// substitute a foreign identity and a recording chown.
type owner struct {
	uid, gid int
	lchown   func(path string, uid, gid int) error
}

// prepareFilesystem runs as root before the harness starts. It lets image-user
// processes traverse to scratch directories under data, and returns stale
// image-user ownership to root so Substrate can delete the DurableDir later.
// Failures are returned as warnings: none of this may block the agent.
func prepareFilesystem(system, data string, identity owner) []error {
	var warnings []error
	// Substrate composes the actor's / with mode 0700 (upstream #2035); a
	// non-root process cannot resolve any path until it is widened.
	if err := ensureMode(system, 0755, false); err != nil {
		warnings = append(warnings, err)
	}
	// Traversal only: listing /data stays private.
	if err := ensureMode(data, 0711, true); err != nil {
		warnings = append(warnings, err)
	}
	for _, name := range runtimeDirectories {
		path := filepath.Join(data, name)
		if err := ensureOwner(path, identity); err != nil {
			warnings = append(warnings, err)
		}
		if err := ensureMode(path, 0700, true); err != nil {
			warnings = append(warnings, err)
		}
	}
	return append(warnings, reclaimForeignEntries(data, identity)...)
}

// ensureMode widens path's permission bits to include mode, or sets them to
// exactly mode when exact is true. It never follows a symlink.
func ensureMode(path string, mode fs.FileMode, exact bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	current := info.Mode().Perm()
	wanted := current | mode
	if exact {
		wanted = mode
	}
	if wanted == current {
		return nil
	}
	if err := os.Chmod(path, wanted); err != nil {
		return fmt.Errorf("set %s mode %#o: %w", path, wanted, err)
	}
	return nil
}

func ensureOwner(path string, identity owner) error {
	uid, gid, err := fileOwner(path)
	if err != nil {
		return err
	}
	if uid == identity.uid && gid == identity.gid {
		return nil
	}
	if err := identity.lchown(path, identity.uid, identity.gid); err != nil {
		return fmt.Errorf("restore %s ownership: %w", path, err)
	}
	return nil
}

// reclaimForeignEntries returns top-level data entries the launcher did not
// create, and that another identity owns, to the launcher's identity, recursively
// and without following symlinks. Substrate's atelet holds no capabilities and
// cannot delete a DurableDir containing them (upstream #2034); dev tooling such
// as dev-postgres leaves them behind when an actor dies mid-run. Ownership of
// the top-level entry is the signal: such tools create their scratch root
// directly under /data and hand it to the image user. No agent process runs
// yet, so nothing can swap paths during the walk.
func reclaimForeignEntries(data string, identity owner) []error {
	entries, err := os.ReadDir(data)
	if err != nil {
		return []error{fmt.Errorf("list %s: %w", data, err)}
	}
	reserved := make(map[string]bool, len(runtimeDirectories))
	for _, name := range runtimeDirectories {
		reserved[name] = true
	}
	var warnings []error
	for _, entry := range entries {
		if reserved[entry.Name()] {
			continue
		}
		top := filepath.Join(data, entry.Name())
		uid, gid, err := fileOwner(top)
		if err != nil {
			warnings = append(warnings, err)
			continue
		}
		if uid == identity.uid && gid == identity.gid {
			continue
		}
		err = filepath.WalkDir(top, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return identity.lchown(path, identity.uid, identity.gid)
		})
		if err != nil {
			warnings = append(warnings, fmt.Errorf("reclaim %s: %w", top, err))
		}
	}
	return warnings
}

func fileOwner(path string) (int, int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("inspect %s: no ownership information", path)
	}
	return int(stat.Uid), int(stat.Gid), nil
}
