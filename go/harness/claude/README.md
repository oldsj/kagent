# Claude Harness

The Claude Harness runs Claude Code as a native Kagent runtime. It compiles an
`AgentTemplate` into Claude Code configuration, runs each turn in a Substrate
Actor, and exposes the result through Kagent's A2A API.

## Code structure

The controller and Actor share only the versioned config contract. Kubernetes
resolution stays in the controller; native process behavior stays in this
harness.

| Path                                                                             | Look here for                                                                                                         |
| -------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| [`../../core/internal/translator/claude`](../../core/internal/translator/claude) | Translating `Harness`, `AgentTemplate`, model, MCP, plugin, and Secret inputs into a runtime revision and warnings    |
| [`config/config.go`](config/config.go)                                           | The versioned JSON contract shared by the compiler and runtime, including defaults and reserved environment variables |
| [`cmd/main.go`](cmd/main.go)                                                     | Actor startup, environment inputs, Claude version validation, continuation-store wiring, and private A2A startup      |
| [`internal/adapter/adapter.go`](internal/adapter/adapter.go)                     | Materializing Claude home, skills, MCP config, and ephemeral provider credentials                                     |
| [`internal/driver`](internal/driver)                                             | Claude CLI arguments, stream-JSON parsing, runtime-event translation, cancellation, and process supervision           |
| [`internal/sdkrunner`](internal/sdkrunner)                                       | The Agent SDK driver's Node bridge supervision, client port, and shared conversation state                            |
| [`sdk-runner`](sdk-runner)                                                       | The Node bridge that holds one live Agent SDK `query()`                                                               |

## Working

- [x] Anthropic API keys and Amazon Bedrock bearer tokens, injected by the gateway
- [x] Streaming text, tool calls, and tool results over A2A
- [x] Task cancellation
- [x] Durable Claude session resume between turns
- [x] Claude Code built-in tools
- [x] Shared local subagents
- [x] Standalone skills and plugin-provided skills
- [x] Direct HTTP and SSE MCP servers with whole-server tool access
- [x] Human-in-the-loop MCP tool approval across DATA-only restart
- [x] Native ask-user questions through durable deferred input

Credentials use [Substrate gateway injection](../../../docs/architecture/credential-injection.md).
AWS IAM keys and Vertex service-account keys require local signing and are rejected
by the compiler. Harness environment entries accept only literal values.

Production runs retain Claude's native retry defaults. E2E fixtures opt into
zero retries through an internal test switch while upstream denial handling
still causes minutes of waiting.

## Turn completion and process supervision

Each native process is one `claude -p` run: the driver sends one user message
and closes stdin. A later turn, or the answer to a deferred input, starts
another process with `--resume`.

**Settings and workspace context.** Claude loads only its user setting source,
the adapter-owned `CLAUDE_CONFIG_DIR`. Project and local settings are not
loaded, because settings can run commands outside any tool call. The checkout
still supplies context: `--add-dir` with
`CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1` loads its `CLAUDE.md` and
`.claude/rules`, and its `.claude/skills` load as a harness-owned plugin
(`workspace:<skill>`) that has no hooks or MCP servers.
`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` keeps provider, credential, proxy and
TLS variables in settings from redirecting model traffic. User-scope hooks are
not disabled: the harness's own approval hooks arrive through `--settings`, and
`disableAllHooks` would stop those too. The `--settings` ask rules still send
protected tools to the permission prompt.

**Headless defaults.** `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` and
`CLAUDE_CODE_DISABLE_CRON=1` are set, and `--disallowedTools` removes
`ScheduleWakeup`, `Monitor`, `CronCreate`, `CronList`, `CronDelete` and
`RemoteTrigger`. A standalone configuration can opt in with
`allow_background_tasks`, `allow_scheduled_tasks` and its own
`disallowed_tools`. The compiler emits none of these, so the defaults apply.

**Activity after a result.** A `result` ends one iteration, not necessarily the
process: a finished background task starts a follow-up iteration, and its
result (origin `task-notification`) supersedes the earlier one. The first
result starts `post_result_grace_millis` (default 120 s). When the grace
expires the driver stops remaining native work and returns the last result,
keeping its success or failure. Later results do not reset the grace. A
deferred-input result always ends its process.

**Background tasks.** The driver tracks background tasks from Claude's
`task_started`, `task_updated`, `task_notification` and
`background_tasks_changed` events. A task is reported once a main-loop model
request starts after it ends and that iteration produces a result. A turn
succeeds only if every background task it started was reported; if Claude
exits with one unreported, the turn fails with a count instead of the earlier
result's success. The grace pauses while any task is unreported, so a long
check and its follow-up finish in the same turn.

**Execution budget.** `turn_timeout_millis` (default two hours) bounds each
native process, including the wait for it to exit. Expiry is a terminal A2A
failure with an execution-budget message. Time spent waiting for a deferred
input is outside any process and is not counted.

**Failures.** A failed Claude result reaches the A2A status as a vetted
message: a diagnostic that might contain a credential is withheld whole, URLs,
email addresses, UUIDs and token-like words are replaced, and long ones are
truncated. The harness logs the same message to stderr. Unexpected runtime
errors keep the generic status.

**Process ownership (Linux).** The harness is a child subreaper and owns one
native tree at a time. Before launch it reaps exited orphans outside its own
process group (for example detached Git maintenance) and refuses to start if
it still has descendants. Launch keeps the leader's pidfd; cleanup signals the
leader and every descendant found through `/proc` ancestry by pidfd, never by
numeric PID or process group, so `setsid`, double-fork and an environment reset
do not escape it. It interrupts, waits the interrupt grace, kills, and reaps
adopted children with `waitid(P_PIDFD)`. A cleanup that cannot prove the tree
is gone fails the turn and keeps the ownership slot, so no later turn starts
over surviving processes; the Actor must then be replaced. Kernels or syscall
filters without pidfd support reject execution. Outside Linux the driver
signals the leader's process group only, which is for development.

**Health artifacts.** When the sink accepts them, each process emits
`kagent.dev/orchestration-health/v1` artifacts: turn start and finish, tool
start and finish with a coarse class, action and workspace-relative path, and
a turn-total usage sample. They carry no arguments, output or prompts. Health
is observation only: a slow or failing consumer never delays, fails or blocks
cancellation of the turn.

## Agent SDK driver

Setting Harness environment `KAGENT_CLAUDE_DRIVER=sdk` (default `process`)
replaces the per-turn `claude -p` process with one long-lived Agent SDK
`query()` per Session. It runs from this image and from the Claude runtime
payload of a composed Session, whose configuration names the payload's Node,
bridge and ICU data instead of the image's. `kagent-claude` stays PID 1 and keeps A2A on `:80`,
`/readyz` on `:8081`, task reporting and the continuation store. A Node child,
`/opt/kagent/sdk-runner/runner.mjs`, holds the `query()` and speaks JSON lines
over stdio; it opens no listener.

- **Restore-safe start.** Node starts on first demand: an A2A turn or a client
  attaching. A golden snapshot taken after `/readyz` never contains it, and a
  DATA restore brings back no process. When the bridge is not running, the next
  demand starts it and resumes the bound Claude session.
- **A2A turns** are user messages with a fresh UUID. A turn ends at the SDK
  `result` whose `user_message_uuids` names that UUID; results of other
  messages pass by.
- **Approvals in an A2A turn are durable.** The bridge loads the same
  `PreToolUse` hooks as the process driver. While an A2A turn is active, a
  protected MCP call or `AskUserQuestion` is deferred: the turn ends with a
  `tool_deferred` result, the driver saves the pending input, stops the bridge
  and returns `input-required`. Nothing then keeps the Actor awake. The answer,
  also after a DATA restore, starts a fresh bridge that resumes the session
  without a new user message; Claude runs the hook for the same call again and
  the hook returns the saved answer. The ask rules still send an allowed call
  to `canUseTool`, where the driver answers from the same saved input and
  refuses any other protected call, as the process driver's permission host
  does. Cancellation refuses the call and interrupts Claude.
- **Other prompts are live.** A protected call in a turn a client started on
  the client port, and any prompt outside the approval boundary (for example
  one raised by the user profile's own permission mode), reaches `canUseTool`
  and waits in the bridge. The first answer, A2A or client, wins, and such a
  prompt is lost if the Actor sleeps.
- **No new message while a call waits.** Claude answers a user message sent
  after a deferred call without running the call. The client port therefore
  refuses user messages until the deferred call has been answered and
  replayed.
- **Client port `:7682`**, reached only through the Substrate router:
  `PUT /bootstrap` sets the Session's client token on first use (and may
  install a Claude user home), `GET /healthz`, and `GET /ws?t=&since=` with
  subprotocol `kagent.sdk-runner.v1`, which replays the in-memory event backlog
  and accepts user messages, interrupts, model, effort, mode and permission
  answers.
- **Profiles.** By default the bridge uses the compiled configuration with the
  harness-owned user scope, the harness's approval hooks, and strict MCP. After
  a bootstrap installs a user home it uses that home with user and project
  setting sources; the user's own hooks run beside the harness's.
- **Activity.** The driver reports busy/idle changes (clients attached, a turn,
  SDK background tasks) to the control plane through `ReportActivity`, and never
  reports before serving a Session. A prompt waiting for a human is not activity
  by itself, so a parked question with no client attached can suspend. A woken
  runtime that starts no turn reports idle once it serves a Session.

The deferral and replay behavior the driver depends on can be checked against
a real CLI and bridge, offline against a fake model API: set
`KAGENT_TEST_CLAUDE_CLI` to the pinned CLI and `KAGENT_TEST_CLAUDE_SDK_RUNNER`
to a `runner.mjs` beside an installed SDK, then run
`go test ./harness/claude/internal/driver -run TestRealBridge`.

The SDK version in [`sdk-runner/package.json`](sdk-runner/package.json) must be
the release built for the pinned Claude CLI (SDK `0.3.N` with Claude Code
`2.1.N`); a test enforces it, and the bridge drives the image's CLI rather than
a bundled one. Telemetry export is not applied in SDK mode, and the event
backlog does not survive an Actor restart.

## Telemetry

The driver passes each prompt to Claude Code on stdin as stream-JSON. When
Claude Code telemetry is on, it holds the prompt until that telemetry has
initialized. Claude Code starts a turn without waiting for its telemetry, and
with first-party Anthropic credentials that initialization first waits on a
managed settings fetch. A turn that starts earlier drops its spans, events and
metrics, and ignores `TRACEPARENT`. The driver therefore adds a loopback
Prometheus metrics reader and sends the prompt once `claude_code.session.count`
appears there, which Claude Code increments only after its providers are
registered. That ordering is a Claude Code implementation detail, verified on
2.1.260, 2.1.282 and 2.1.285.

The wait is bounded at 10 seconds, so a stalled settings fetch delays a turn by
at most that much. The driver then sends the prompt and records a
`kagent.claude.telemetry_not_ready` event on the turn's `invoke_agent` span, as
it also does when it cannot reserve the loopback port. Claude Code stops waiting
for the fetch after 30 seconds, so in a turn that runs past then, spans started
after its telemetry initializes become roots of new traces rather than being
absent.

The gate delays new stdin prompts only. Deferred replay starts automatically on
`--resume`, so the gate cannot ensure telemetry is initialized before replay.

## Human-in-the-loop approval flow

Claude runs in print mode with compiler-owned `PreToolUse` hooks. Protected MCP
calls and `AskUserQuestion` return `permissionDecision: "defer"` before execution.
Claude exits with the pending call preserved in its transcript. The driver saves
A2A task ownership and the pending input, then returns `input-required` after
reaping the process.

After DATA restore, the shared executor recovers ownership and the harness starts
`claude -p --resume <session-id>` without adding a new user prompt. The hook reads
the saved approval or answer. Explicit ask rules also consult the private MCP
permission host; it returns only that matching saved decision. Ask-user answers
are supplied through `updatedInput`.

Cancellation resolves the native deferred call with a stop decision before
writing a tombstone.

The native [defer documentation](https://code.claude.com/docs/en/hooks#defer-a-tool-call-for-later)
describes the transcript-based continuation.

## Key limitations

- **Tool parenting is not retained across DATA suspension.** Resumed invocations
  link to the task's original invocation. Deferred tool spans use the resumed
  invocation and exclude approval wait time. Other tool spans still open when
  input is published are settled as unfinished; previous tool parents are not
  restored. Parenting within an execution segment is retained.
  Deferred span duration includes replay startup and is not exact tool-only time.

- **Parallel approvals are not supported.** Protected calls that Claude cannot
  defer in a parallel batch are denied through the permission fallback. A
  remaining single call can still be deferred and approved.

## Planned / not yet supported

- [ ] Enforced selection of individual tools from an MCP server
- [ ] Dedicated subagents running in separate Sessions
- [ ] Skills, MCP tools, and nested subagents on local subagents
- [ ] Configuring Claude Code permission mode and trust boundary in Harness CRD

## Example usage

The platform-managed `Harness` selects the Claude runtime, and the `Agent`
references it while defining behavior inline. The referenced ModelConfig and
RemoteMCPServer must already exist in `kagent`. The installed release supplies
the default Harness image. To override it, set `workload.image` to a full
`@sha256:`-pinned reference.

```yaml
apiVersion: api.kagent.dev/v1alpha3
kind: Harness
metadata:
  name: claude
  namespace: kagent
spec:
  claude: {}
  substrate:
    workerPoolRef:
      name: kagent-default
    snapshotPolicy:
      location: s3://ate-snapshots/kagent/
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  name: kagent-claude
  namespace: kagent
spec:
  template:
    inline:
      description: test
      modelConfig:
        name: bedrock-claude # Assuming you have created a modelconfig using Bedrock Anthropic
      systemPrompt: |
        Follow the selected skill and use the configured MCP tool.
      tools:
        - mcp:
            server:
              kind: RemoteMCPServer
              name: kagent-tool-server
      plugins:
        - source:
            git:
              url: https://github.com/agentplugins/agent-plugins-example.git
              commit: 5f3f5084a821aefa792e79500dd8f0462ab83473
          skills:
            - migrate-agent-plugin
  harnessRef:
    name: claude
```
