package chatgptrefresh

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type initializeParams struct {
	ClientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type accountParams struct {
	RefreshToken bool `json:"refreshToken"`
}
type rpcResponse struct {
	ID    int             `json:"id"`
	Error json.RawMessage `json:"error"`
}

// Official rust-v0.148.0 account/read -> refresh_token_if_requested ->
// AuthManager::refresh_token -> refresh_and_persist_chatgpt_token. No thread
// or turn requests are sent. Discard all upstream diagnostics, which can echo
// credentials. Each process gets only our isolated, memory-backed CODEX_HOME.
func refreshCLI(ctx context.Context, executable, home string) error {
	version := exec.CommandContext(ctx, executable, "--version")
	version.Env = cliEnvironment(home)
	version.Stderr = io.Discard
	output, err := version.Output()
	if err != nil || strings.TrimSpace(string(output)) != "codex-cli "+codexVersion {
		return errOperation
	}
	command := exec.CommandContext(ctx, executable, "app-server", "--strict-config", "--stdio")
	command.Dir, command.Env = home, cliEnvironment(home)
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		return errOperation
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return errOperation
	}
	if command.Start() != nil {
		return errOperation
	}
	defer func() { _ = stdin.Close(); _ = command.Process.Kill(); _ = command.Wait() }()
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	var params initializeParams
	params.ClientInfo.Name, params.ClientInfo.Version = "kagent-credential-refresh", "1"
	if encoder.Encode(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: params}) != nil || awaitResponse(scanner, 1) != nil {
		return errOperation
	}
	if encoder.Encode(rpcRequest{JSONRPC: "2.0", Method: "initialized"}) != nil {
		return errOperation
	}
	if encoder.Encode(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "account/read", Params: accountParams{RefreshToken: true}}) != nil {
		return errOperation
	}
	return awaitResponse(scanner, 2)
}

func awaitResponse(scanner *bufio.Scanner, id int) error {
	for scanner.Scan() {
		var response rpcResponse
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			return errOperation
		}
		if response.ID != id {
			continue
		}
		if len(response.Error) > 0 && string(response.Error) != "null" {
			return errOperation
		}
		return nil
	}
	return errOperation
}

func cliEnvironment(home string) []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "CODEX_HOME=" + home, "RUST_LOG=off"}
}
