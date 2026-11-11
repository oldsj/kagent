package sdkrunner

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeLauncherArguments(t *testing.T) {
	scenarios := []struct {
		name       string
		icuDataDir string
		want       string
	}{
		{name: "Given no ICU data directory, when the bridge is launched, assert that Node receives only the script", want: "/bridge/runner.mjs"},
		{name: "Given a payload ICU data directory, when the bridge is launched, assert that Node receives it as a flag before the script", icuDataDir: "/payload/share/icu", want: "--icu-data-dir=/payload/share/icu /bridge/runner.mjs"},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			node := filepath.Join(dir, "node")
			if err := os.WriteFile(node, []byte("#!/bin/sh\necho \"$@\"\necho \"NODE_ICU_DATA=${NODE_ICU_DATA-unset}\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			launcher := NodeLauncher{Node: node, Script: "/bridge/runner.mjs", ICUDataDir: scenario.icuDataDir, Dir: dir, Env: []string{"PATH=/usr/bin:/bin"}}

			process, err := launcher.Launch(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(process.Stdout())
			if err != nil {
				t.Fatal(err)
			}
			_ = process.Stop(time.Second)

			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			if len(lines) != 2 || lines[0] != scenario.want || lines[1] != "NODE_ICU_DATA=unset" {
				t.Fatalf("bridge saw %q, want arguments %q and no NODE_ICU_DATA", lines, scenario.want)
			}
		})
	}
}
