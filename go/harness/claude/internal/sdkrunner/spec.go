// Package sdkrunner runs Claude Code through the Agent SDK bridge
// (sdk-runner/runner.mjs) as one long-lived conversation per Actor, and serves
// that conversation to WebSocket clients on the client port.
//
// kagent-claude remains the Actor's process: it keeps the private A2A
// endpoint, readiness, TaskStore, Git preparation and the continuation store.
// The Node bridge is a child started lazily on first demand, so a golden
// snapshot taken right after readiness never contains it, and a bridge that
// dies (for example after a memory-snapshot restore) is started again on the
// next demand with the bound continuation.
package sdkrunner

import (
	"encoding/json"
	"fmt"
)

// Profile selects the Claude configuration policy for the bridge.
type Profile string

const (
	// ProfileManaged applies the compiled harness policy, as the process
	// driver does: harness-owned user scope, no hooks, strict MCP config.
	ProfileManaged Profile = "managed"
	// ProfileUser runs with a user-supplied Claude home delivered once through
	// /bootstrap: user and project settings, hooks, skills and plugins.
	ProfileUser Profile = "user"
)

// Spec is the bridge's start operation. Go compiles it from the same adapter
// output the process driver uses; the bridge maps it onto SDK options.
type Spec struct {
	Cwd                   string                     `json:"cwd"`
	Executable            string                     `json:"executable"`
	Env                   map[string]string          `json:"env"`
	Model                 string                     `json:"model,omitempty"`
	Effort                string                     `json:"effort,omitempty"`
	Resume                string                     `json:"resume,omitempty"`
	SettingSources        []string                   `json:"settingSources"`
	Settings              string                     `json:"settings,omitempty"`
	PermissionMode        string                     `json:"permissionMode,omitempty"`
	StrictMCPConfig       bool                       `json:"strictMcpConfig,omitempty"`
	MCPServers            map[string]json.RawMessage `json:"mcpServers,omitempty"`
	DisallowedTools       []string                   `json:"disallowedTools,omitempty"`
	AdditionalDirectories []string                   `json:"additionalDirectories,omitempty"`
	PluginDirs            []string                   `json:"pluginDirs,omitempty"`
	Agents                json.RawMessage            `json:"agents,omitempty"`
	AppendSystemPrompt    string                     `json:"appendSystemPrompt,omitempty"`
}

// EnvMap converts a process environment to the bridge's map form. Later
// entries win, as they do for exec.
func EnvMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, item := range environment {
		for i := 0; i < len(item); i++ {
			if item[i] == '=' {
				result[item[:i]] = item[i+1:]
				break
			}
		}
	}
	return result
}

func (s Spec) validate() error {
	if s.Cwd == "" || s.Executable == "" {
		return fmt.Errorf("sdk runner spec requires cwd and executable")
	}
	return nil
}
