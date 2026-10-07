package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Manifest identifies the payload ABI and every shipped regular file.
type Manifest struct {
	Schema         uint32            `json:"schema"`
	Provider       string            `json:"provider"`
	Platform       string            `json:"platform"`
	Libc           string            `json:"libc"`
	CLIVersion     string            `json:"cliVersion"`
	ArtifactSHA256 string            `json:"artifactSHA256"`
	Files          map[string]string `json:"files"`
}

// BuildManifest inventories a completed payload. Symlinks are dereferenced by
// the build so the final image needs no paths outside the mounted closure.
func BuildManifest(root, provider string) (Manifest, error) {
	release := LockedRelease(provider)
	manifest := Manifest{Schema: Schema, Provider: provider, Platform: runtime.GOOS + "/" + runtime.GOARCH, Libc: "musl-private", CLIVersion: release.Version, ArtifactSHA256: release.Checksums[runtime.GOARCH], Files: map[string]string{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if name == "manifest.json" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("payload file %s is not regular", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		manifest.Files[filepath.ToSlash(name)] = hex.EncodeToString(sum[:])
		return nil
	})
	return manifest, err
}

// ValidateManifest verifies ABI identity and payload bytes before executing any
// dynamic helper. The static launcher itself is mounted from the pinned image.
func ValidateManifest(root, platform string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return manifest, fmt.Errorf("read runtime manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode runtime manifest: %w", err)
	}
	if manifest.Schema != Schema || manifest.Platform != platform || manifest.Libc != "musl-private" || (manifest.Provider != "claude" && manifest.Provider != "codex") {
		return manifest, fmt.Errorf("incompatible runtime manifest: schema=%d platform=%q libc=%q provider=%q (host %s)", manifest.Schema, manifest.Platform, manifest.Libc, manifest.Provider, platform)
	}
	release := LockedRelease(manifest.Provider)
	arch := filepath.Base(platform)
	if manifest.CLIVersion != release.Version || manifest.ArtifactSHA256 != release.Checksums[arch] {
		return manifest, fmt.Errorf("runtime manifest does not match the CLI release lock")
	}
	for _, required := range []string{"bin/launch", "bin/kagent-" + manifest.Provider, "bin/" + manifest.Provider, "libexec/" + manifest.Provider, "bin/bash", "bin/git"} {
		if manifest.Files[required] == "" {
			return manifest, fmt.Errorf("runtime manifest is missing %s", required)
		}
	}
	if manifest.Provider == "claude" && manifest.Files["bin/rg"] == "" {
		return manifest, fmt.Errorf("runtime manifest is missing bin/rg")
	}
	if manifest.Provider == "codex" && manifest.Files["bin/kagent-credential-refresh"] == "" {
		return manifest, fmt.Errorf("runtime manifest is missing credential refresh helper")
	}
	for name, checksum := range manifest.Files {
		if !fs.ValidPath(name) || name == "manifest.json" {
			return manifest, fmt.Errorf("invalid runtime manifest path %q", name)
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return manifest, fmt.Errorf("runtime manifest file %q is missing or not regular", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return manifest, fmt.Errorf("read runtime file %s: %w", name, err)
		}
		sum := sha256.Sum256(data)
		if checksum != hex.EncodeToString(sum[:]) {
			return manifest, fmt.Errorf("runtime checksum mismatch: %s", name)
		}
	}
	return manifest, nil
}
