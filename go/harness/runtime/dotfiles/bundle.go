// Package dotfiles lays a Session's agent dotfiles out read-only in the CLI's
// user configuration directory before turns run. The bundle is the uncompressed
// tar the Session was created with; the runtime fetches it through the egress
// gateway, verifies its digest and limits, and executes nothing from it.
package dotfiles

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	apidotfiles "github.com/kagent-dev/kagent/go/api/dotfiles"
)

// File is one regular file of a bundle, at its path relative to the dotfiles root.
type File struct {
	Path       string
	Executable bool
	Data       []byte
}

// Digest is data's "sha256:<hex>".
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Unpack verifies bundle against digest and returns its files. It refuses a
// bundle over the limits, an entry that is not a regular file, and a path the
// layout does not take.
func Unpack(bundle []byte, digest string) ([]File, error) {
	if len(bundle) > apidotfiles.MaxBundleBytes {
		return nil, errors.New("dotfiles bundle is over the size limit")
	}
	if Digest(bundle) != digest {
		return nil, errors.New("dotfiles bundle does not match its digest")
	}
	reader := tar.NewReader(bytes.NewReader(bundle))
	var files []File
	seen := map[string]bool{}
	total := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read dotfiles bundle: %w", err)
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("dotfiles entry %q is not a regular file", header.Name)
		}
		if !allowed(header.Name) {
			return nil, fmt.Errorf("dotfiles entry %q is not a path the layout takes", header.Name)
		}
		if seen[header.Name] {
			return nil, fmt.Errorf("dotfiles entry %q appears twice", header.Name)
		}
		seen[header.Name] = true
		if len(files) == apidotfiles.MaxFiles {
			return nil, errors.New("dotfiles bundle has too many files")
		}
		if header.Size < 0 || header.Size > apidotfiles.MaxFileBytes || total+int(header.Size) > apidotfiles.MaxTotalBytes {
			return nil, errors.New("dotfiles bundle is over the size limits")
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size))
		if err != nil || int64(len(data)) != header.Size {
			return nil, fmt.Errorf("read dotfiles entry %q: %w", header.Name, errors.Join(err, io.ErrUnexpectedEOF))
		}
		total += len(data)
		files = append(files, File{Path: header.Name, Executable: header.Mode&0o111 != 0, Data: data})
	}
	return files, nil
}

// allowed reports whether p is a clean relative path of the shape a bundle
// holds: AGENTS.md, CLAUDE.md, skills/<name>/..., agents/<name>.md,
// commands/<name>.md or claude/settings.json.
func allowed(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." ||
		strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	parts := strings.Split(p, "/")
	switch {
	case p == "AGENTS.md" || p == "CLAUDE.md" || p == ClaudeSettingsPath:
		return true
	case parts[0] == "skills":
		return len(parts) >= 3
	case parts[0] == "agents" || parts[0] == "commands":
		return len(parts) == 2 && strings.HasSuffix(parts[1], ".md")
	}
	return false
}
