package translator

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

const (
	testDevelopmentImage = "registry.example/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testPayloadImage     = "registry.example/runtime-claude@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestComposeRevision(t *testing.T) {
	t.Run("Given a compiled Claude revision on an arm64 pool, when it is composed with an arm64 selection, assert that the launcher runs the payload CLI from the development image", func(t *testing.T) {
		base := claudeBaseRevision(t)

		composed, err := ComposeRevision(base, claudeSelection())
		if err != nil {
			t.Fatalf("ComposeRevision() error = %v", err)
		}

		if composed.Image != testDevelopmentImage {
			t.Errorf("Image = %q, want the development image", composed.Image)
		}
		if len(composed.Command) != 1 || composed.Command[0] != payload.Root+"/bin/launch" {
			t.Errorf("Command = %v, want the payload launcher", composed.Command)
		}
		config, err := claudeconfig.Parse(composed.ConfigJSON)
		if err != nil {
			t.Fatalf("parse composed config: %v", err)
		}
		if config.ClaudeExecutable != payload.Root+"/bin/claude" || config.ExpectedClaudeVersion != "2.1.285" {
			t.Errorf("config CLI = %q@%q, want the payload CLI at the catalog version", config.ClaudeExecutable, config.ExpectedClaudeVersion)
		}
		if got := environmentValue(composed.Environment, payload.PlatformEnvironment); got != "linux/arm64" {
			t.Errorf("%s = %q, want linux/arm64", payload.PlatformEnvironment, got)
		}
		if want := (claudeconfig.SDKRunner{Node: payload.SDKNode, Script: payload.SDKRunnerScript, ICUDataDir: payload.SDKICUData}); config.SDKRunner == nil || *config.SDKRunner != want {
			t.Errorf("config SDK runner = %+v, want the payload bridge %+v", config.SDKRunner, want)
		}
		if base.Composition != nil || base.Image == composed.Image {
			t.Error("composition modified the base revision")
		}
	})

	t.Run("Given a compiled Claude revision that selects the SDK driver, when it is composed, assert that the driver selection is kept for the payload bridge", func(t *testing.T) {
		base := claudeBaseRevision(t)
		base.Environment = append(base.Environment, corev1.EnvVar{Name: "KAGENT_CLAUDE_DRIVER", Value: "sdk"})

		composed, err := ComposeRevision(base, claudeSelection())
		if err != nil {
			t.Fatalf("ComposeRevision() error = %v", err)
		}

		if got := environmentValue(composed.Environment, "KAGENT_CLAUDE_DRIVER"); got != "sdk" {
			t.Errorf("KAGENT_CLAUDE_DRIVER = %q, want sdk", got)
		}
	})

	t.Run("Given a revision sized by its Harness, when it is composed with a development image, assert that the composed Session keeps the Harness's CPU and memory limits", func(t *testing.T) {
		base := claudeBaseRevision(t)
		base.CPU, base.Memory = "4", "8Gi"

		composed, err := ComposeRevision(base, claudeSelection())
		if err != nil {
			t.Fatalf("ComposeRevision() error = %v", err)
		}

		if composed.CPU != "4" || composed.Memory != "8Gi" {
			t.Errorf("composed limits = %s CPU, %s memory; want the Harness's 4 CPU and 8Gi", composed.CPU, composed.Memory)
		}
	})

	t.Run("Given a single-image revision, when its digest is compared with its composed variant, assert that the identities differ and the base digest ignores the composition fields", func(t *testing.T) {
		base := claudeBaseRevision(t)
		before, err := base.Digest()
		if err != nil {
			t.Fatal(err)
		}
		base.Harness, base.WorkerPoolPlatform = "", ""
		withoutDerived, err := base.Digest()
		if err != nil {
			t.Fatal(err)
		}
		base = claudeBaseRevision(t)
		composed, err := ComposeRevision(base, claudeSelection())
		if err != nil {
			t.Fatal(err)
		}
		composedID, err := composed.Digest()
		if err != nil {
			t.Fatal(err)
		}

		if before != withoutDerived {
			t.Error("derived Harness and WorkerPoolPlatform changed the digest")
		}
		if composedID == before {
			t.Error("composed revision has the base revision's digest")
		}
	})

	scenarios := []struct {
		name   string
		mutate func(*Revision, *Composition)
		want   string
	}{
		{"a selection for another provider", func(_ *Revision, c *Composition) { c.Provider = HarnessTypeCodex }, "differs from the compiled"},
		{"a pool without an architecture pin", func(r *Revision, _ *Composition) { r.WorkerPoolPlatform = "" }, "must pin kubernetes.io/arch"},
		{"a pool pinned to another architecture", func(r *Revision, _ *Composition) { r.WorkerPoolPlatform = "linux/amd64" }, "must pin kubernetes.io/arch"},
		{"a tag-pinned development image", func(_ *Revision, c *Composition) { c.DevelopmentImage = "registry.example/dev:latest" }, "digest-pinned"},
		{"a blank policy identity", func(_ *Revision, c *Composition) { c.PolicyIdentity = " " }, "policy identity"},
		{"an unsupported payload schema", func(_ *Revision, c *Composition) { c.Schema = payload.Schema + 1 }, "payload schema"},
		{"Harness environment that overrides PATH", func(r *Revision, _ *Composition) {
			r.Environment = append(r.Environment, corev1.EnvVar{Name: "PATH", Value: "/usr/bin"})
		}, "conflicts with the composed runtime"},
		{"Harness environment that points into the payload", func(r *Revision, _ *Composition) {
			r.Environment = append(r.Environment, corev1.EnvVar{Name: "TOOL", Value: payload.Root + "/bin/git"})
		}, "reserved runtime paths"},
		{"Harness environment that turns the Claude sandbox off", func(r *Revision, _ *Composition) {
			r.Environment = append(r.Environment, corev1.EnvVar{Name: claudeconfig.SandboxEnvName, Value: "0"})
		}, "conflicts with the composed runtime"},
		{"a Harness command override", func(r *Revision, _ *Composition) { r.Command = []string{"/bin/sh"} }, "launch command"},
		{"an already composed revision", func(r *Revision, c *Composition) { selection := *c; r.Composition = &selection }, "already composed"},
	}
	for _, scenario := range scenarios {
		t.Run("Given "+scenario.name+", when the revision is composed, assert that composition is rejected", func(t *testing.T) {
			base, selection := claudeBaseRevision(t), claudeSelection()
			scenario.mutate(&base, &selection)

			_, err := ComposeRevision(base, selection)

			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("ComposeRevision() error = %v, want it to mention %q", err, scenario.want)
			}
		})
	}
}

func TestWorkerPoolPlatform(t *testing.T) {
	scenarios := []struct {
		name     string
		selector map[string]string
		want     string
	}{
		{"an arm64 pin", map[string]string{"kubernetes.io/arch": "arm64"}, "linux/arm64"},
		{"an amd64 pin on Linux", map[string]string{"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux"}, "linux/amd64"},
		{"no architecture pin", map[string]string{"pool": "general"}, ""},
		{"a non-Linux OS pin", map[string]string{"kubernetes.io/arch": "amd64", "kubernetes.io/os": "windows"}, ""},
		{"an unsupported architecture", map[string]string{"kubernetes.io/arch": "s390x"}, ""},
	}
	for _, scenario := range scenarios {
		t.Run("Given a WorkerPool node selector with "+scenario.name+", when its platform is derived, assert that it is "+strings.ReplaceAll(scenario.want, "/", " "), func(t *testing.T) {
			if got := WorkerPoolPlatform(scenario.selector); got != scenario.want {
				t.Fatalf("WorkerPoolPlatform() = %q, want %q", got, scenario.want)
			}
		})
	}
}

func claudeBaseRevision(t *testing.T) Revision {
	t.Helper()
	config, err := json.Marshal(claudeconfig.Production("claude-sonnet-5-5", "be helpful"))
	if err != nil {
		t.Fatal(err)
	}
	return Revision{
		AgentName: "coder", AgentUID: "uid", Namespace: "mainloop",
		Image:       "ghcr.example/kagent-claude@sha256:3333333333333333333333333333333333333333333333333333333333333333",
		Environment: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}, {Name: claudeconfig.SandboxEnvName, Value: "1"}}, ConfigJSON: config,
		WorkerPoolName: "arm64", Harness: HarnessTypeClaude, WorkerPoolPlatform: "linux/arm64",
	}
}

func claudeSelection() Composition {
	return Composition{
		DevelopmentImage: testDevelopmentImage, PayloadImage: testPayloadImage, CLIVersion: "2.1.285",
		Platform: "linux/arm64", PolicyIdentity: "project-a/v1", Provider: HarnessTypeClaude, Schema: payload.Schema,
	}
}

func environmentValue(environment []corev1.EnvVar, name string) string {
	for _, variable := range environment {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
}
