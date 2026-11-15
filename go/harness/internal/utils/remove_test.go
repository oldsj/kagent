package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTreeRemovesAReadOnlyTreeAndNotASymlinkTarget(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "skill")
	if err := os.MkdirAll(filepath.Join(tree, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "scripts", "run.sh"), []byte("x"), 0o555); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(tree, "scripts"), tree} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveTree(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tree remains: %v", err)
	}

	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(link); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("the symlink's target was touched: %v, %v", info, err)
	}
	if err := RemoveTree(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("missing path: %v", err)
	}
}
