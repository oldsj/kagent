package driver

// settingSources loads only Claude Code's user scope, the adapter-owned
// CLAUDE_CONFIG_DIR. Project and local settings can run commands outside any
// tool call (settings env reaches processes Claude starts, and helper keys
// such as otelHeadersHelper run commands), so a checkout must not supply
// them. The checkout's CLAUDE.md and .claude/rules load through --add-dir
// (see Args), and the adapter supplies its skills as a plugin.
//
// The user scope can hold hooks. The harness cannot disable them, because its
// own approval and cancellation hooks arrive through --settings and
// disableAllHooks would stop those too. The scope is written only by the
// Actor's own agent, and the approval boundary does not rest on hooks alone:
// the --settings ask rules still route protected tools to the permission
// prompt, which Claude evaluates before allow rules from any source.
const settingSources = "user"
