package dotfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// manifest records what the last layout wrote, so the next one removes exactly
// that and nothing the CLI or the agent put beside it.
type manifest struct {
	Digest  string   `json:"digest"`
	Entries []string `json:"entries"`
}

func readManifest(stateDir string) manifest {
	var m manifest
	data, err := os.ReadFile(filepath.Join(stateDir, "manifest.json"))
	if err == nil && json.Unmarshal(data, &m) == nil {
		return m
	}
	return manifest{}
}

func writeManifest(stateDir string, m manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return utils.ReplacePrivateFile(filepath.Join(stateDir, "manifest.json"), data)
}

// lay removes the entries the previous layout wrote, then writes files' entries
// under t.Dir read-only: files 0444, or 0555 when executable, and directories
// 0555. The manifest is written before the files, so a layout cut short is
// removed whole by the next one. It returns the entries written.
func lay(t Target, stateDir, digest string, files []File) ([]string, error) {
	if !filepath.IsAbs(t.Dir) {
		return nil, fmt.Errorf("dotfiles directory %q must be absolute", t.Dir)
	}
	if err := utils.EnsurePrivateDir(t.Dir); err != nil {
		return nil, err
	}
	if err := utils.EnsurePrivateDir(stateDir); err != nil {
		return nil, err
	}
	for _, e := range readManifest(stateDir).Entries {
		if !safeEntry(e) {
			continue
		}
		if err := removeEntry(filepath.Join(t.Dir, e)); err != nil {
			return nil, fmt.Errorf("remove dotfiles entry %q: %w", e, err)
		}
	}
	groups := map[string][]File{}
	for _, f := range t.place(files) {
		groups[entry(f.Path)] = append(groups[entry(f.Path)], f)
	}
	var entries []string
	for e := range groups {
		if t.yieldsSkills && strings.HasPrefix(e, "skills/") {
			if _, err := os.Lstat(filepath.Join(t.Dir, e)); err == nil {
				continue
			}
		}
		entries = append(entries, e)
	}
	slices.Sort(entries)
	if err := writeManifest(stateDir, manifest{Digest: digest, Entries: entries}); err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := writeEntry(t.Dir, e, groups[e]); err != nil {
			return nil, fmt.Errorf("lay out dotfiles entry %q: %w", e, err)
		}
	}
	return entries, nil
}

// intact reports whether every entry still exists as the layout left it in
// kind: something removed it, such as Codex reconciling its skills root.
func intact(dir string, entries []string) bool {
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(dir, e))
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func safeEntry(e string) bool {
	return allowed(e) || (strings.HasPrefix(e, "skills/") && strings.Count(e, "/") == 1 && allowed(e+"/SKILL.md"))
}

func writeEntry(dir, e string, files []File) error {
	if parent := filepath.Dir(e); parent != "." {
		if err := realDir(filepath.Join(dir, parent)); err != nil {
			return err
		}
	}
	root := filepath.Join(dir, e)
	// Whatever is there was not written by the last layout: the agent's own
	// file, or a leftover. The dotfiles entry replaces it.
	if err := removeEntry(root); err != nil {
		return err
	}
	var dirs []string
	for _, f := range files {
		target := filepath.Join(dir, f.Path)
		for d := filepath.Dir(target); d != filepath.Dir(root) && !slices.Contains(dirs, d); d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if f.Executable {
			mode = 0o555
		}
		// O_EXCL refuses a symlink planted in the fresh directory.
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = file.Write(f.Data)
		if err = errors.Join(err, file.Close()); err != nil {
			return err
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
	}
	// Deepest first, so no directory is closed before its children are done.
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return err
		}
	}
	return nil
}

// realDir makes path a directory the layout owns writes into, replacing a
// symlink or file an agent may have put there so nothing is written through it.
func realDir(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(path, 0o700)
	case err != nil:
		return err
	case info.IsDir():
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Mkdir(path, 0o700)
}

// removeEntry removes path, read-only or not, without following a symlink.
func removeEntry(path string) error { return utils.RemoveTree(path) }
