//go:build linux

package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	v2translator "github.com/kagent-dev/kagent/go/core/internal/translator"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	claudeexecutor "github.com/kagent-dev/kagent/go/harness/claude/executor"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"istio.io/istio/pkg/kube/krt"
)

func TestCompilerRenderedDefaultsReachLaunchedCommand(t *testing.T) {
	for _, composed := range []bool{false, true} {
		name := "stock"
		if composed {
			name = "composed"
		}
		t.Run(name, func(t *testing.T) {
			model := v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-test", APIKeySecret: "model-auth", APIKeySecretKey: "api-key"}
			input, reader := testInput(t, model, map[string][]byte{"api-key": []byte("fake-key")})
			compiled, err := NewCompiler(krt.TestingDummyContext{}, reader).Compile(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			revision := compiled.Revision
			revision.AgentName = input.AgentName
			if composed {
				revision, err = v2translator.ComposeRevision(revision, v2translator.Composition{
					DevelopmentImage: "example.com/devenv@sha256:" + strings.Repeat("a", 64),
					PayloadImage:     "example.com/payload@sha256:" + strings.Repeat("b", 64),
					CLIVersion:       claudeconfig.PinnedClaudeVersion, Platform: "linux/amd64", PolicyIdentity: "test", Provider: v2translator.HarnessTypeClaude, Schema: payload.Schema,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			id, err := revision.Digest()
			if err != nil {
				t.Fatal(err)
			}
			actor, err := substrate.ActorTemplateForRevision(&revision, id)
			if err != nil {
				t.Fatal(err)
			}
			var rendered string
			for _, variable := range actor.Containers[0].Env {
				if variable.Name == "KAGENT_CONFIG_JSON" {
					rendered = variable.Value
				}
			}
			if rendered != string(revision.ConfigJSON) {
				t.Fatal("Actor rendered a different configuration")
			}
			cfg, err := claudeconfig.Parse([]byte(rendered))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PostResultGrace() != claudeconfig.DefaultPostResultGrace || cfg.TurnTimeout() != claudeconfig.DefaultTurnTimeout {
				t.Fatalf("compiled execution limits = %#v", cfg)
			}
			dir := t.TempDir()
			capture := filepath.Join(dir, "launch")
			executable := filepath.Join(dir, "claude")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + claudeconfig.PinnedClaudeVersion + "'; exit 0; fi\n" + `cat >/dev/null
{
printf '%s\n' "$CLAUDE_CODE_DISABLE_BACKGROUND_TASKS" "$CLAUDE_CODE_DISABLE_CRON"
printf '%s\n' "$@"
} > "$CAPTURE"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}'
`
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			// Only replace the bundled executable with a shell fake; retain every
			// policy field from the controller-rendered configuration.
			cfg.ClaudeExecutable = executable
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			environment := []string{"PATH=/usr/bin:/bin", "CAPTURE=" + capture, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=0", "CLAUDE_CODE_DISABLE_CRON=0"}
			for _, variable := range revision.Environment {
				environment = append(environment, variable.Name+"="+variable.Value)
			}
			executor, closer, err := claudeexecutor.New(t.Context(), claudeexecutor.Config{ConfigJSON: raw, DataDir: filepath.Join(dir, "data"), Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
			message.TaskID, message.ContextID = "review-launch", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			request := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
			var completed int
			for event, err := range executor.Execute(t.Context(), request) {
				if err != nil {
					t.Fatal(err)
				}
				if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State == a2atype.TaskStateCompleted {
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("completed count=%d", completed)
			}
			launch, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(launch), "1\n1\n") || !strings.Contains(string(launch), "--disallowedTools\nScheduleWakeup,Monitor,CronCreate,CronList,CronDelete,RemoteTrigger\n") {
				t.Fatalf("unsafe launch:\n%s", launch)
			}
		})
	}
}
