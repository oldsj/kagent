package payload

import (
	"debug/elf"
	"fmt"
	"path/filepath"
	"strings"
)

// NativeCommand preserves vendor executable bytes (Bun embeds data at ELF file
// offsets that patchelf can invalidate). Dynamic CLIs use the private loader;
// static CLIs execute directly. Project tools never inherit a library override.
func NativeCommand(root, provider, arch string, args []string) (string, []string, error) {
	path := filepath.Join(root, "libexec", provider)
	file, err := elf.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("inspect bundled CLI: %w", err)
	}
	defer file.Close()
	dynamic := false
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			dynamic = true
		}
	}
	if !dynamic {
		return path, append([]string{path}, args...), nil
	}
	var loader string
	switch arch {
	case "amd64":
		loader = "ld-musl-x86_64.so.1"
	case "arm64":
		loader = "ld-musl-aarch64.so.1"
	default:
		return "", nil, fmt.Errorf("unsupported native CLI architecture %s", arch)
	}
	executable := filepath.Join(root, "lib", loader)
	return executable, append([]string{executable, "--library-path", root + "/lib", path}, args...), nil
}

// NativeEnvironment removes dynamic-loader overrides from the native CLI.
func NativeEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "LD_") {
			result = append(result, entry)
		}
	}
	return result
}
