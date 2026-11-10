package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/codex/config"
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
