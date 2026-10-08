package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

func main() {
	var err error
	if provider := filepath.Base(os.Args[0]); provider == "claude" || provider == "codex" {
		var path string
		var args []string
		path, args, err = payload.NativeCommand(payload.Root, provider, runtime.GOARCH, os.Args[1:])
		if err == nil {
			err = syscall.Exec(path, args, payload.NativeEnvironment(os.Environ()))
		}
	} else if len(os.Args) == 3 && os.Args[1] == "--write-manifest" {
		var manifest payload.Manifest
		manifest, err = payload.BuildManifest(payload.Root, os.Args[2])
		if err == nil {
			var data []byte
			data, err = json.MarshalIndent(manifest, "", "  ")
			if err == nil {
				err = os.WriteFile(payload.Root+"/manifest.json", append(data, '\n'), 0644)
			}
		}
	} else if len(os.Args) == 1 || (len(os.Args) == 2 && os.Args[1] == "--check") {
		err = payload.Launch(payload.Root, "/", "/data", runtime.GOOS+"/"+runtime.GOARCH, len(os.Args) == 2, os.Environ(), syscall.Exec)
	} else {
		err = fmt.Errorf("usage: launch [--check]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime launch:", err)
		os.Exit(1)
	}
}
