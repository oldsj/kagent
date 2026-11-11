// Package payload defines the native runtime payload contract and release pins.
// A composed Session runs a development image with this payload mounted
// read-only at Root; the payload supplies the harness, its CLI and helpers.
package payload

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Root is where Substrate mounts the payload image volume.
const Root = "/opt/mainloop-runtime"

// SDKNode and SDKRunnerScript are the Claude payload's Agent SDK bridge. They
// sit in libexec, off the PATH the launcher builds, so the payload's Node
// never shadows the development image's own. Alpine's Node loads ICU data at
// run time from SDKICUData, which a development image does not provide.
const (
	SDKNode         = Root + "/libexec/node"
	SDKRunnerScript = Root + "/libexec/sdk-runner/runner.mjs"
	SDKICUData      = Root + "/share/icu"
)

// Schema versions the payload layout and manifest contract.
const Schema = 1

// PlatformEnvironment carries the trusted platform selection into the launcher.
const PlatformEnvironment = "MAINLOOP_RUNTIME_PLATFORM"

//go:embed runtime-lock.json
var lockJSON []byte

// Release pins one provider CLI version and its per-architecture checksums.
type Release struct {
	Version   string            `json:"version"`
	Checksums map[string]string `json:"checksums"`
}

// LockedRelease returns the single source of provider CLI versions and checksums.
func LockedRelease(provider string) Release {
	var releases map[string]Release
	if err := json.Unmarshal(lockJSON, &releases); err != nil {
		panic(fmt.Sprintf("invalid embedded runtime lock: %v", err))
	}
	release, ok := releases[provider]
	if !ok {
		panic("unknown runtime provider: " + provider)
	}
	return release
}
