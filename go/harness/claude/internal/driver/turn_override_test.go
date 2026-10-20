package driver

import (
	"slices"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestArgsAppliesTurnModelAndEffortOverride(t *testing.T) {
	for name, test := range map[string]struct {
		config    ProcessConfig
		turn      runtime.Turn
		wantModel string
		wantFlags []string
	}{
		"no override uses configured model": {
			config: ProcessConfig{Model: "claude-sonnet-5"}, wantModel: "claude-sonnet-5",
		},
		"turn model overrides configured model": {
			config: ProcessConfig{Model: "claude-sonnet-5"}, turn: runtime.Turn{Model: "claude-opus-5"}, wantModel: "claude-opus-5",
		},
		"turn model without a configured default": {
			turn: runtime.Turn{Model: "claude-opus-5"}, wantModel: "claude-opus-5",
		},
		"turn effort is passed through": {
			turn: runtime.Turn{Effort: "high"}, wantFlags: []string{"--effort", "high"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := NewProcessDriver(test.config).Args(test.turn)
			if test.wantModel != "" {
				model := slices.Index(args, "--model")
				if model < 0 || model+1 >= len(args) || args[model+1] != test.wantModel {
					t.Fatalf("arguments do not pass model %q: %q", test.wantModel, args)
				}
			} else if slices.Contains(args, "--model") {
				t.Fatalf("arguments pass --model with no configured or overridden model: %q", args)
			}
			for i := 0; i+1 < len(test.wantFlags); i += 2 {
				flag, value := slices.Index(args, test.wantFlags[i]), test.wantFlags[i+1]
				if flag < 0 || flag+1 >= len(args) || args[flag+1] != value {
					t.Fatalf("arguments do not pass %s %q: %q", test.wantFlags[i], value, args)
				}
			}
			if test.turn.Effort == "" && slices.Contains(args, "--effort") {
				t.Fatalf("arguments pass --effort with no turn override: %q", args)
			}
		})
	}
}
