package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// codexCLIEnvName selects a real Codex executable for the opt-in test that
// checks the trust entry against it. Point it at the pinned release
// (PinnedCodexVersion); the test is skipped when unset.
const codexCLIEnvName = "KAGENT_TEST_CODEX_CLI"

// TestRenderConfigAppendsWorkspaceTrust pins the exact bytes the pinned Codex
// CLI appends to config.toml the first time thread/start runs in an untrusted
// Git checkout. TestPinnedCodexLeavesTrustedWorkspaceUnchanged checks them
// against the real binary.
func TestRenderConfigAppendsWorkspaceTrust(t *testing.T) {
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.Provider = config.Provider{Name: "openai"}
	contents, err := renderConfig(cfg, t.TempDir(), "/data/workspace", false)
	if err != nil {
		t.Fatal(err)
	}
	const want = "\n[projects.\"/data/workspace\"]\ntrust_level = \"trusted\"\n"
	if !strings.HasSuffix(string(contents), want) {
		t.Fatalf("generated Codex configuration does not end with Codex's workspace trust entry:\n%s", contents)
	}
}

// TestPinnedCodexLeavesTrustedWorkspaceUnchanged drives the real Codex App
// Server through the thread/start call ProcessDriver.Run makes, in a Git
// checkout, before and after New rewrites config.toml as it does on every
// harness start. Codex must leave the rendered config.toml byte-identical, so
// the trust entry Codex would otherwise append is already there and survives a
// restart. (thread/resume needs a thread that ran a turn, so it is not driven
// here; it does not add a missing trust entry.)
func TestPinnedCodexLeavesTrustedWorkspaceUnchanged(t *testing.T) {
	executable := os.Getenv(codexCLIEnvName)
	if executable == "" {
		t.Skipf("%s is not set", codexCLIEnvName)
	}
	durable := filepath.Join(t.TempDir(), "data")
	workspaceDir := filepath.Join(durable, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", workspaceDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	cfg := config.Production("gpt-5.2-codex", "work carefully")
	cfg.CodexExecutable, cfg.StrictVersion = executable, false
	cfg.Provider = config.Provider{Name: "openai"}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(durable, "codex", "config.toml")
	render := func() []byte {
		t.Helper()
		if _, err := New(t.Context(), Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable, Environment: os.Environ()}); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		return contents
	}
	checkUnchanged := func(step string, before []byte) {
		t.Helper()
		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("Codex changed config.toml after %s:\nbefore:\n%s\nafter:\n%s", step, before, after)
		}
	}
	params := map[string]any{
		"cwd": workspaceDir, "model": cfg.Model, "modelProvider": "kagent-openai",
		"approvalPolicy": map[string]any{"granular": map[string]bool{
			"sandbox_approval": false, "rules": false, "skill_approval": false, "request_permissions": false, "mcp_elicitations": true,
		}},
		"sandbox": "danger-full-access", "developerInstructions": cfg.DeveloperInstruction,
	}

	before := render()
	callCodexThread(t, executable, durable, workspaceDir, "thread/start", params)
	checkUnchanged("thread/start", before)

	// A harness restart rewrites config.toml before the next turn.
	before = render()
	callCodexThread(t, executable, durable, workspaceDir, "thread/start", params)
	checkUnchanged("a restart and another thread/start", before)
}

// callCodexThread runs one App Server process through initialize and a single
// thread call, then stops it, as one ProcessDriver turn does before turn/start.
func callCodexThread(t *testing.T, executable, durable, workspaceDir, method string, params map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "app-server", "--strict-config", "--stdio")
	command.Dir = workspaceDir
	command.Env = append(os.Environ(), "CODEX_HOME="+filepath.Join(durable, "codex"), "OPENAI_API_KEY=test")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	reader := bufio.NewReader(stdout)
	write := func(message map[string]any) {
		t.Helper()
		line, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id int) json.RawMessage {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read Codex App Server response for id %d: %v: %s", id, err, stderr.String())
			}
			var response struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatalf("decode Codex App Server response: %v: %s", err, line)
			}
			if response.ID == nil || *response.ID != id || response.Method != "" {
				continue // a notification or a server request
			}
			if len(response.Error) != 0 {
				t.Fatalf("Codex App Server returned an error for %d: %s", id, response.Error)
			}
			return response.Result
		}
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]string{"name": "kagent-test", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true},
	}})
	read(1)
	write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})
	return read(2)
}

// TestPinnedCodexKeepsConfigAcrossTurns is the regression test for the
// second-turn failure of prepared Codex workspaces: every turn checks that
// config.toml is unchanged, and the first thread/start used to append a
// [projects] trust table, so every later turn failed "Workspace preparation is
// incomplete." It drives the real Codex App Server through the production
// driver for three turns against a local fake Responses API: a new thread, then
// two resumes, with the per-turn model and effort override Mainloop sends, as
// one long-lived actor runs them. After each turn the check every prepared turn
// runs must pass. Both provider shapes Mainloop uses are covered: an API key and
// a ChatGPT subscription.
func TestPinnedCodexKeepsConfigAcrossTurns(t *testing.T) {
	executable := os.Getenv(codexCLIEnvName)
	if executable == "" {
		t.Skipf("%s is not set", codexCLIEnvName)
	}
	server := fakeResponsesAPI(t)
	for _, test := range []struct {
		name     string
		provider config.Provider
	}{
		{name: "OpenAI", provider: config.Provider{Name: "openai", BaseURL: server.URL + "/v1"}},
		{name: "ChatGPT", provider: config.Provider{Name: "chatgpt", BaseURL: server.URL + "/v1", AccountID: "acct-test", ResponsesTransport: "https"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Given a prepared Git workspace whose actor started once
			durable := filepath.Join(t.TempDir(), "data")
			workspaceDir := filepath.Join(durable, "workspace")
			if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "init", "-q", workspaceDir).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, output)
			}
			cfg := config.Production("gpt-5.2-codex", "work carefully")
			cfg.CodexExecutable, cfg.StrictVersion = executable, false
			cfg.Provider = test.provider
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			input := Input{ConfigJSON: raw, Workspace: workspaceDir, DurableDir: durable, Environment: append(os.Environ(), "OPENAI_API_KEY=test")}
			driver, err := New(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}

			// When it runs a new thread and two resumes, the first two with a per-turn model and effort
			thread := ""
			for i, turn := range []runtime.Turn{
				{Prompt: "first", Model: "gpt-5.2-codex", Effort: "high"},
				{Prompt: "second", Model: "gpt-5.2-codex", Effort: "low"},
				{Prompt: "third"},
			} {
				turn.ContinuationID = thread
				sink := &threadSink{}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				outcome, err := driver.Run(ctx, turn, sink)
				cancel()
				if err != nil || outcome.Failure != nil || sink.thread == "" {
					t.Fatalf("turn %d: %+v, %v (thread %q)", i+1, outcome, err, sink.thread)
				}
				if thread != "" && sink.thread != thread {
					t.Fatalf("turn %d resumed thread %q, want %q", i+1, sink.thread, thread)
				}
				thread = sink.thread

				// Then config.toml passes the check the next turn runs
				if err := checkInstalledConfig(input); err != nil {
					contents, _ := os.ReadFile(filepath.Join(durable, "codex", "config.toml"))
					t.Fatalf("after turn %d: %v; config.toml is now:\n%s", i+1, err, contents)
				}
			}
			if n := server.Requests(); n < 3 {
				t.Fatalf("the fake Responses API saw %d turns, want at least 3", n)
			}
		})
	}
}

// threadSink records the native thread a turn ran on.
type threadSink struct{ thread string }

func (s *threadSink) SessionStarted(e runtime.SessionStarted) error {
	s.thread = e.ContinuationID
	return nil
}
func (*threadSink) TextDelta(runtime.TextDelta) error   { return nil }
func (*threadSink) ToolCall(runtime.ToolCall) error     { return nil }
func (*threadSink) ToolResult(runtime.ToolResult) error { return nil }
func (*threadSink) ModelCall(runtime.ModelCall)         {}

// responsesAPI is a local stand-in for the Responses API: every turn gets one
// assistant message, so a turn completes without a model or credentials.
type responsesAPI struct {
	*httptest.Server
	requests atomic.Int32
}

func (s *responsesAPI) Requests() int { return int(s.requests.Load()) }

func fakeResponsesAPI(t *testing.T) *responsesAPI {
	t.Helper()
	api := &responsesAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/v1/responses" {
			// Model listing and anything else: nothing to offer.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[],"data":[]}`))
			return
		}
		id := fmt.Sprintf("resp_%d", api.requests.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": id}},
			{"type": "response.output_item.done", "item": map[string]any{
				"type": "message", "role": "assistant", "id": "msg_" + id,
				"content": []map[string]any{{"type": "output_text", "text": "done"}},
			}},
			{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{
				"input_tokens": 1, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": 2,
			}}},
		} {
			data, err := json.Marshal(event)
			if err != nil {
				panic(err)
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	t.Cleanup(api.Close)
	return api
}
