//go:build linux

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
)

func TestConfiguredExecutionLimitsReachDriver(t *testing.T) {
	for _, test := range []struct {
		name           string
		grace, ceiling int
		state          a2atype.TaskState
	}{
		{name: "grace", grace: 100, ceiling: 10_000, state: a2atype.TaskStateCompleted},
		{name: "ceiling", grace: 10_000, ceiling: 100, state: a2atype.TaskStateFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Production("fixture-model", "")
			cfg.ClaudeExecutable = filepath.Join(dir, "claude")
			cfg.PostResultGraceMillis, cfg.TurnTimeoutMillis, cfg.InterruptGraceMillis = test.grace, test.ceiling, 20
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + cfg.ExpectedClaudeVersion + "'; exit 0; fi\n" + `cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success"}'
trap '' INT
exec sleep 30
`
			if err := os.WriteFile(cfg.ClaudeExecutable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			executor, closer, err := New(t.Context(), Config{ConfigJSON: raw, DataDir: filepath.Join(dir, "data"), Environment: []string{"PATH=/usr/bin:/bin"}})
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))
			message.TaskID, message.ContextID = "configured-limit", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			terminal := 0
			for event, err := range executor.Execute(ctx, &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}) {
				if err != nil {
					t.Fatal(err)
				}
				if status, ok := event.(*a2atype.TaskStatusUpdateEvent); ok && status.Status.State.Terminal() {
					terminal++
					if status.Status.State != test.state {
						t.Fatalf("state = %s, want %s", status.Status.State, test.state)
					}
					if test.state == a2atype.TaskStateFailed && (status.Status.Message == nil || status.Status.Message.Parts[0].Text() != "Claude execution budget exceeded (approval wait time excluded)") {
						t.Fatalf("failure = %#v", status.Status.Message)
					}
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal count = %d", terminal)
			}
		})
	}
}
