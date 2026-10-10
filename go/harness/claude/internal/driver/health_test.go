package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"github.com/stretchr/testify/require"
)

func TestClaudeHealthThroughA2ASnapshot(t *testing.T) {
	fixture, err := os.ReadFile("../../testdata/stream-health.jsonl")
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stream"), fixture, 0600))
	executable := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ncat >/dev/null\ncat stream\n"), 0700))
	runner := NewProcessDriver(ProcessConfig{Executable: executable, Workspace: dir, ExpectedVersion: pinnedClaudeVersion, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second})
	executor, err := runtimea2a.New(runner, failureContinuation{}, tracing.RuntimeTelemetry{})
	require.NoError(t, err)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("fixture"))
	message.TaskID, message.ContextID = "health-task", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	req := &a2asrv.ExecutorContext{TaskID: message.TaskID, ContextID: message.ContextID, Message: message}
	task := &a2a.Task{ID: req.TaskID, ContextID: req.ContextID}
	var health []runtime.HealthEvent
	for event, err := range executor.Execute(t.Context(), req) {
		require.NoError(t, err)
		task, err = a2aevent.ApplyUpdate(task, event)
		require.NoError(t, err)
	}
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	for _, artifact := range task.Artifacts {
		if artifact.Name != runtime.HealthSchema {
			continue
		}
		data, err := json.Marshal(artifact.Parts[0].Data())
		require.NoError(t, err)
		require.NotContains(t, string(data), "PRIVATE")
		require.NotContains(t, string(data), "mcp__fixture")
		var event runtime.HealthEvent
		require.NoError(t, json.Unmarshal(data, &event))
		require.Equal(t, string(artifact.ID), event.EventID)
		require.Equal(t, string(task.ID), event.A2ATaskID)
		require.Equal(t, string(task.ContextID), event.RuntimeSessionID)
		health = append(health, event)
	}
	require.Len(t, health, 9)
	var failures, usage int
	for i, event := range health {
		require.Equal(t, int64(i+1), event.Sequence)
		require.Equal(t, health[0].ProducerEpoch, event.ProducerEpoch)
		if event.Tool != nil && event.Tool.Outcome == "error" {
			failures++
			require.Equal(t, "mcp_error", event.Tool.Category)
			require.Equal(t, health[2].Tool.OperationKey, event.Tool.OperationKey)
		}
		if event.Usage != nil {
			usage++
			require.EqualValues(t, 3, *event.Usage.InputTokens)
			require.EqualValues(t, 2, *event.Usage.OutputTokens)
		}
	}
	require.Equal(t, 3, failures)
	require.Equal(t, 1, usage, "replayed result must not create another usage sample")
	require.Equal(t, "complete", health[len(health)-1].Turn.Coverage)
}

func TestHealthPathsAndUnknownUsage(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"/repo/src/a.go", "src/a.go"}, {"src/a.go", "src/a.go"}, {"/other/a.go", ""}, {"../outside", ""}, {".env.local", ""}, {".ssh/key", ""}, {"credentials.json", ""},
	} {
		t.Run(test.path, func(t *testing.T) {
			got := safeHealthPath("/repo", test.path)
			if test.want == "" {
				require.Nil(t, got)
			} else {
				require.NotNil(t, got)
				require.Equal(t, test.want, *got)
			}
		})
	}
	var events []runtime.HealthEvent
	input := []byte(`{"type":"result","subtype":"success"}` + "\n" + `{"type":"result","uuid":"second","subtype":"success","usage":{"input_tokens":4,"output_tokens":1}}` + "\n")
	require.NoError(t, ParseJSONL(bytes.NewReader(input), 4096, func(e Event) error {
		if e.Kind == EventHealth {
			events = append(events, e.Health)
		}
		return nil
	}))
	require.Nil(t, events[1].Usage.InputTokens)
	require.Equal(t, "partial", events[2].Usage.Coverage)
	require.Equal(t, int64(2), events[2].Usage.Revision)
	require.Equal(t, events[1].Usage.SampleKey, events[2].Usage.SampleKey)
	var incomplete []runtime.HealthEvent
	err := ParseJSONL(strings.NewReader(`{"type":"system","subtype":"init","session_id":"fixture"}`+"\n"), 4096, func(e Event) error {
		if e.Kind == EventHealth {
			incomplete = append(incomplete, e.Health)
		}
		return nil
	})
	require.Error(t, err)
	require.Len(t, incomplete, 1)
	require.Equal(t, "partial", incomplete[0].Turn.Coverage)
}

type isolatedHealthSink struct {
	*recordingSink
	fail    error
	entered chan struct{}
	release chan struct{}
}

func (s *isolatedHealthSink) Health(runtime.HealthEvent) error {
	if s.entered != nil {
		select {
		case <-s.entered:
		default:
			close(s.entered)
		}
		<-s.release
	}
	return s.fail
}

func healthIsolationDriver(t *testing.T) *ProcessDriver {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"done\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}'\nsleep 1\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
	return NewProcessDriver(ProcessConfig{Executable: executable, Workspace: dir, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 20 * time.Millisecond, PostResultGrace: 50 * time.Millisecond})
}

// Health is an observation: its failure must never change the turn.
func TestHealthSinkFailureDoesNotFailTurn(t *testing.T) {
	sink := &isolatedHealthSink{recordingSink: &recordingSink{}, fail: errors.New("fixture health sink unavailable")}
	outcome, err := healthIsolationDriver(t).Run(t.Context(), runtime.Turn{Prompt: "fixture"}, sink)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Nil(t, outcome.Pending)
	require.Len(t, sink.sessions, 1, "the turn's own events still reach the sink")
}

// A stalled health consumer must not hold the turn or its cancellation.
func TestHealthSinkStallDoesNotBlockCompletionOrCancellation(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		sink := &isolatedHealthSink{recordingSink: &recordingSink{}, entered: make(chan struct{}), release: make(chan struct{})}
		defer close(sink.release)
		outcome, err := healthIsolationDriver(t).Run(t.Context(), runtime.Turn{Prompt: "fixture"}, sink)
		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
	})
	t.Run("cancellation", func(t *testing.T) {
		sink := &isolatedHealthSink{recordingSink: &recordingSink{}, entered: make(chan struct{}), release: make(chan struct{})}
		defer close(sink.release)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		runner := healthIsolationDriver(t)
		go func() {
			_, err := runner.Run(ctx, runtime.Turn{Prompt: "fixture"}, sink)
			done <- err
		}()
		select {
		case <-sink.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("fixture never reached the health sink")
		}
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled turn remained blocked in Health")
		}
	})
}
