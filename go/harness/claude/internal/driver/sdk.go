package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// approvalServerName is the process driver's loopback approval MCP server.
// The SDK bridge answers permission prompts through canUseTool instead, so the
// server is left out of the bridge's MCP configuration.
const approvalServerName = "kagent_hitl"

// SDKSpec compiles the Agent SDK bridge's start operation from the same
// adapter output the process driver turns into CLI arguments.
func (d *ProcessDriver) SDKSpec(profile sdkrunner.Profile, home string) (sdkrunner.Spec, error) {
	executable, err := exec.LookPath(d.config.Executable)
	if err != nil {
		return sdkrunner.Spec{}, fmt.Errorf("find Claude executable %q: %w", d.config.Executable, err)
	}
	env := sdkrunner.EnvMap(d.config.Environment)
	// Session state events drive activity reports and client turn state.
	env["CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"] = "1"
	env["CLAUDE_AGENT_SDK_CLIENT_APP"] = "kagent-sdk-runner/1"
	spec := sdkrunner.Spec{
		Cwd: d.config.Workspace, Executable: executable, Env: env,
		Model: d.config.Model, DisallowedTools: d.config.DisallowedTools,
		PluginDirs: d.config.PluginDirs,
	}
	if d.config.AppendSystemPromptFile != "" {
		prompt, err := os.ReadFile(d.config.AppendSystemPromptFile)
		if err != nil {
			return sdkrunner.Spec{}, fmt.Errorf("read Claude system prompt: %w", err)
		}
		spec.AppendSystemPrompt = string(prompt)
	}
	// The process driver's settings file carries the hooks that defer a tool
	// and end its process. The bridge keeps one live query and answers prompts
	// through canUseTool, so it takes only the ask rules.
	var ask []string
	if d.config.ApprovalBroker != nil {
		ask = d.config.ApprovalBroker.askRules()
	}
	if d.config.Workspace != "" {
		// As in Args: loads the checkout's CLAUDE.md and .claude/rules.
		spec.AdditionalDirectories = []string{d.config.Workspace}
	}
	if d.config.AgentsJSON != "" {
		spec.Agents = json.RawMessage(d.config.AgentsJSON)
	}
	if d.config.MCPConfigPath != "" {
		raw, err := os.ReadFile(d.config.MCPConfigPath)
		if err != nil {
			return sdkrunner.Spec{}, fmt.Errorf("read Claude MCP configuration: %w", err)
		}
		var mcp struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &mcp); err != nil {
			return sdkrunner.Spec{}, fmt.Errorf("decode Claude MCP configuration: %w", err)
		}
		delete(mcp.MCPServers, approvalServerName)
		if len(mcp.MCPServers) != 0 {
			spec.MCPServers = mcp.MCPServers
		}
	}
	switch profile {
	case sdkrunner.ProfileManaged:
		// Harness-owned user scope only. No harness hook is needed here, so
		// every hook is disabled; compiled ask rules reach canUseTool.
		spec.SettingSources = []string{settingSources}
		settings, err := sdkSettingsJSON(ask, true)
		if err != nil {
			return sdkrunner.Spec{}, err
		}
		spec.Settings = settings
		spec.StrictMCPConfig = true
		spec.PermissionMode = "bypassPermissions"
	case sdkrunner.ProfileUser:
		// The user's own Claude home: their settings, hooks, skills, plugins.
		// Provider, credentials, proxy and CA stay owned by kagent through
		// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST in the environment.
		delete(env, claudeconfig.ClaudeConfigDirEnvName)
		env["HOME"] = home
		spec.SettingSources = []string{"user", "project"}
		spec.PermissionMode = "auto"
		// The compiled ask rules stay; the user's own hooks run.
		settings, err := sdkSettingsJSON(ask, false)
		if err != nil {
			return sdkrunner.Spec{}, err
		}
		spec.Settings = settings
	default:
		return sdkrunner.Spec{}, fmt.Errorf("unknown sdk runner profile %q", profile)
	}
	return spec, nil
}

// sdkSettingsJSON is the bridge's --settings layer: the compiled ask rules,
// and for the managed profile a block on every hook.
func sdkSettingsJSON(ask []string, disableHooks bool) (string, error) {
	settings := map[string]any{}
	if disableHooks {
		settings["disableAllHooks"] = true
	}
	if len(ask) != 0 {
		settings["permissions"] = map[string]any{"ask": ask}
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encode Claude SDK runner settings: %w", err)
	}
	return string(encoded), nil
}

// SDKDriver runs A2A turns as user messages in the Actor's live SDK
// conversation. Clients on the runner port share that conversation.
type SDKDriver struct {
	*ProcessDriver
	session *sdkrunner.Session
}

// NewSDKDriver wraps a configured process driver, whose configuration and
// approval broker it keeps, around the shared SDK session.
func NewSDKDriver(base *ProcessDriver, session *sdkrunner.Session) *SDKDriver {
	return &SDKDriver{ProcessDriver: base, session: session}
}

var errRunnerExited = errors.New("the Claude SDK runner exited during the turn")

// Run sends the prompt and translates the conversation's events until the
// result that consumed it.
func (d *SDKDriver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if strings.TrimSpace(turn.Prompt) == "" {
		return runtime.Outcome{}, fmt.Errorf("Claude prompt is required")
	}
	sub, err := d.session.BeginTurn()
	if err != nil {
		return runtime.Outcome{}, err
	}
	t := &sdkTurn{driver: d, sub: sub, uuid: uuid.NewString(), budget: newActiveBudget(d.config.TurnTimeout)}
	owned := false
	defer func() {
		if !owned {
			d.session.EndTurn(sub)
		}
	}()
	if err := d.session.Start(ctx); err != nil {
		return runtime.Outcome{}, fmt.Errorf("start Claude SDK runner: %w", err)
	}
	if err := d.session.Submit(ctx, turn.Prompt, t.uuid, sdkrunner.OriginA2A); err != nil {
		return runtime.Outcome{}, fmt.Errorf("send prompt to Claude SDK runner: %w", err)
	}
	outcome, err := t.consume(ctx, sink)
	owned = err == nil && outcome.Pending != nil
	return outcome, err
}

type sdkTurn struct {
	driver         *SDKDriver
	sub            *sdkrunner.Subscription
	uuid           string
	budget         *activeBudget
	parser         *parser
	sessionStarted bool
	pending        *sdkrunner.Perm
}

func (t *sdkTurn) consume(ctx context.Context, sink runtime.EventSink) (runtime.Outcome, error) {
	observer, observed := sink.(runtime.HealthSink)
	health := startHealthPublisher(observer)
	defer health.close(ctx)
	if t.parser == nil {
		t.parser = newParser(time.Now)
		if observed {
			t.parser.health = newHealthProducer(t.driver.config.Workspace, t.driver.config.ExpectedVersion, nil)
		}
	}
	if id := t.driver.session.SessionID(); id != "" && !t.sessionStarted {
		t.sessionStarted = true
		if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: id}); err != nil {
			return runtime.Outcome{}, err
		}
	}
	for {
		item, err := t.next(ctx)
		if err != nil {
			if errors.Is(err, errExecutionBudgetExceeded) {
				_ = t.interrupt(context.WithoutCancel(ctx))
				return runtime.Outcome{}, runtime.NewTerminalFailure("Claude execution budget exceeded (approval wait time excluded)", err)
			}
			if ctx.Err() != nil {
				// Cancellation: interrupt the native turn and wait, bounded, for
				// its result so the conversation is idle for the next turn.
				return runtime.Outcome{}, errors.Join(ctx.Err(), t.interrupt(context.WithoutCancel(ctx)))
			}
			return runtime.Outcome{}, err
		}
		switch item.Kind {
		case sdkrunner.ItemExited:
			return runtime.Outcome{}, errRunnerExited
		case sdkrunner.ItemPerm:
			t.pending = item.Perm
			t.budget.pause()
			return runtime.Outcome{Pending: &sdkPendingTurn{turn: t}}, nil
		case sdkrunner.ItemPermDone:
			continue
		case sdkrunner.ItemSDK:
			outcome, done, err := t.translate(ctx, item.Raw, sink, health)
			if err != nil || done {
				return outcome, err
			}
		}
	}
}

func (t *sdkTurn) next(ctx context.Context) (sdkrunner.Item, error) {
	if t.budget.expired() {
		return sdkrunner.Item{}, errExecutionBudgetExceeded
	}
	waitCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	expiry := t.budget.done()
	go func() {
		select {
		case <-expiry:
			cancel(errExecutionBudgetExceeded)
		case <-waitCtx.Done():
		}
	}()
	item, err := t.sub.Next(waitCtx)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(waitCtx), errExecutionBudgetExceeded) {
		return item, errExecutionBudgetExceeded
	}
	return item, err
}

// translate maps one SDK message. A result ends the turn only if it consumed
// this turn's prompt; results of client turns pass by.
func (t *sdkTurn) translate(ctx context.Context, raw json.RawMessage, sink runtime.EventSink, health *healthPublisher) (runtime.Outcome, bool, error) {
	if isResult, uuids := sdkrunner.ResultUUIDs(raw); isResult && !slices.Contains(uuids, t.uuid) {
		return runtime.Outcome{}, false, nil
	}
	var outcome *runtime.Outcome
	err := t.parser.parseLine(ctx, raw, func(event Event) error {
		switch event.Kind {
		case EventHealth:
			health.publish(event.Health)
			return nil
		case EventBackgroundTasks:
			return nil // The session tracks background work for activity.
		case EventDeferred:
			return fmt.Errorf("Claude SDK runner deferred a tool; its settings install no deferral hook")
		case EventSessionStarted:
			if t.sessionStarted {
				return nil
			}
			t.sessionStarted = true
		}
		result, err := emitEvent(event, sink, false)
		if result != nil {
			outcome = result
		}
		return err
	})
	if err != nil {
		return runtime.Outcome{}, true, err
	}
	if outcome != nil {
		return *outcome, true, nil
	}
	return runtime.Outcome{}, false, nil
}

// interrupt asks the SDK to stop the turn and waits up to the interrupt grace
// for its result. A bridge that does not settle is stopped, so the next demand
// starts a fresh bridge that resumes the conversation.
func (t *sdkTurn) interrupt(ctx context.Context) error {
	if t.pending != nil {
		_, _ = t.driver.session.AnswerPerm(t.pending.ID, "deny", false, "The task was canceled.", sdkrunner.OriginA2A)
		t.pending = nil
	}
	if err := t.driver.session.Control(ctx, "interrupt", "", ""); err != nil {
		return t.driver.session.Stop()
	}
	grace := t.driver.config.InterruptGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	for {
		item, err := t.sub.Next(waitCtx)
		if err != nil {
			logging.FromContext(ctx).WarnContext(ctx, "claude sdk runner did not settle after interrupt; stopping it")
			return t.driver.session.Stop()
		}
		if item.Kind == sdkrunner.ItemExited {
			return nil
		}
		if item.Kind == sdkrunner.ItemSDK {
			if isResult, uuids := sdkrunner.ResultUUIDs(item.Raw); isResult && slices.Contains(uuids, t.uuid) {
				return nil
			}
		}
	}
}

// sdkPendingTurn holds the A2A turn while a permission request waits. Either
// the A2A response or a client's answer resolves it; the first wins.
type sdkPendingTurn struct {
	turn *sdkTurn
}

func (p *sdkPendingTurn) Request() runtime.InputRequest {
	perm := p.turn.pending
	return &runtime.ApprovalRequest{
		ID: perm.ID, CallID: perm.ToolUseID, Name: perm.ToolName, Args: perm.Input,
		Hint: "Claude requires approval before calling " + perm.ToolName + ".",
	}
}

func (p *sdkPendingTurn) Resume(ctx context.Context, response runtime.InputResponse, sink runtime.EventSink) (outcome runtime.Outcome, err error) {
	t := p.turn
	owned := false
	defer func() {
		if !owned {
			t.driver.session.EndTurn(t.sub)
		}
	}()
	decision, ok := response.(*runtime.ApprovalDecision)
	if !ok {
		return runtime.Outcome{}, fmt.Errorf("Claude is waiting for a structured tool approval response")
	}
	if decision.ID != t.pending.ID {
		return runtime.Outcome{}, fmt.Errorf("tool approval response ID %q does not match pending ID %q", decision.ID, t.pending.ID)
	}
	behavior, message := "allow", ""
	if !decision.Approved {
		behavior, message = "deny", decision.RejectionReason
	}
	// False means a client already answered; the turn continues either way.
	if _, err := t.driver.session.AnswerPerm(t.pending.ID, behavior, false, message, sdkrunner.OriginA2A); err != nil {
		return runtime.Outcome{}, err
	}
	t.pending = nil
	t.budget.resume()
	outcome, err = t.consume(ctx, sink)
	owned = err == nil && outcome.Pending != nil
	return outcome, err
}

func (p *sdkPendingTurn) Cancel(ctx context.Context, _ runtime.ModelCallSink) error {
	defer p.turn.driver.session.EndTurn(p.turn.sub)
	return p.turn.interrupt(ctx)
}

var _ runtime.PendingTurn = (*sdkPendingTurn)(nil)
