// Package payload defines the native runtime payload contract and release pins.
package payload

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

const Root = "/opt/mainloop-runtime"
const Schema = 1
const PlatformEnvironment = "MAINLOOP_RUNTIME_PLATFORM"

//go:embed runtime-lock.json
var lockJSON []byte

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
