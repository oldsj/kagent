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
	"sync"
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
		// Harness-owned user scope only, as for the process driver.
		spec.SettingSources = []string{settingSources}
		settings, err := d.sdkSettingsJSON(true)
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
		// The harness hooks and ask rules stay; the user's own hooks run too.
		settings, err := d.sdkSettingsJSON(false)
		if err != nil {
			return sdkrunner.Spec{}, err
		}
		spec.Settings = settings
	default:
		return sdkrunner.Spec{}, fmt.Errorf("unknown sdk runner profile %q", profile)
	}
	return spec, nil
}

// sdkSettingsJSON is the bridge's --settings layer. With an approval boundary
// it is the process driver's own: the hooks that defer a protected call and
// answer its replay, and the ask rules that fail closed when a call cannot be
// deferred. Without one, the managed profile disables every hook.
func (d *ProcessDriver) sdkSettingsJSON(managed bool) (string, error) {
	if d.config.ApprovalBroker != nil {
		settings, err := d.config.ApprovalBroker.SettingsJSON()
		if err != nil {
			return "", fmt.Errorf("encode Claude SDK runner settings: %w", err)
		}
		return string(settings), nil
	}
	if managed {
		return `{"disableAllHooks":true}`, nil
	}
	return "{}", nil
}

// SDKDriver runs A2A turns as user messages in the Actor's live SDK
// conversation. Clients on the runner port share that conversation.
type SDKDriver struct {
	*ProcessDriver
	session *sdkrunner.Session

	// steerMu orders steer acceptance against the end of the turn that owns
	// the steers, and guards every turn's steers.
	steerMu sync.Mutex
	// steering is the A2A turn that accepts steers, or nil. A replay, a turn
	// waiting on a permission prompt, and a turn that is ending accept none.
	steering *sdkTurn
}

// NewSDKDriver wraps a configured process driver, whose configuration and
// approval broker it keeps, around the shared SDK session.
func NewSDKDriver(base *ProcessDriver, session *sdkrunner.Session) *SDKDriver {
	if broker := base.config.ApprovalBroker; broker != nil {
		// A deferred call ends its turn and must be parked durably. Only an A2A
		// turn can do that; a client's own turn keeps its live prompt.
		broker.DeferNewCallsWhen(session.TurnActive)
	}
	return &SDKDriver{ProcessDriver: base, session: session}
}

// RestorePending recovers a deferred tool call after a DATA restore. Its answer
// replays through the SDK conversation, not through a `claude -p` process.
func (d *SDKDriver) RestorePending() (string, string, runtime.PendingTurn, error) {
	return d.restorePending(d.replay)
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
	t := &sdkTurn{driver: d, sub: sub, turn: turn, uuid: uuid.NewString(), budget: newActiveBudget(d.config.TurnTimeout)}
	owned := false
	defer func() {
		if !owned {
			d.session.EndTurn(sub)
		}
	}()
	if err := d.session.Start(ctx); err != nil {
		return runtime.Outcome{}, fmt.Errorf("start Claude SDK runner: %w", err)
	}
	// Overrides are strictly per turn, as in the process driver: every turn
	// sets both controls, so a turn without an override runs on the configured
	// model and the model's default effort, never on the previous turn's
	// override or a setting a runner-port client chose.
	model := turn.Model
	if model == "" {
		model = d.config.Model
	}
	for _, control := range []struct{ what, value string }{{"model", model}, {"effort", turn.Effort}} {
		if err := d.session.Control(ctx, control.what, control.value, ""); err != nil {
			return runtime.Outcome{}, fmt.Errorf("set Claude SDK runner %s: %w", control.what, err)
		}
	}
	if err := d.session.SubmitMessage(ctx, turn.Prompt, t.uuid, turn.MessageID, sdkrunner.OriginA2A); err != nil {
		return runtime.Outcome{}, fmt.Errorf("send prompt to Claude SDK runner: %w", err)
	}
	d.openSteering(t)
	outcome, err := t.consume(ctx, sink)
	owned = err == nil && livePrompt(outcome)
	t.settleSteering(owned, sink)
	return outcome, err
}

// Steer submits a message into the running A2A turn of steer.TaskID. The
// turn then owns it: Claude reads it at its next step, and the turn ends only
// at a result that has consumed it too. Acceptance holds steerMu, which the
// turn takes to decide its end, so a steer is never accepted by a turn that
// has already decided to end.
func (d *SDKDriver) Steer(ctx context.Context, steer runtime.Steer) error {
	if strings.TrimSpace(steer.Text) == "" || steer.MessageID == "" {
		return fmt.Errorf("a steer requires text and a message ID")
	}
	d.steerMu.Lock()
	defer d.steerMu.Unlock()
	t := d.steering
	if t == nil || t.turn.TaskID != steer.TaskID {
		return runtime.SteerTaskNotRunning
	}
	if t.awaitingInput {
		return runtime.SteerInputRequired
	}
	if steer.MessageID == t.turn.MessageID || slices.ContainsFunc(t.steers, func(s ownedSteer) bool { return s.messageID == steer.MessageID }) {
		return nil
	}
	id := uuid.NewString()
	if err := d.session.SubmitMessage(ctx, steer.Text, id, steer.MessageID, sdkrunner.OriginA2A); err != nil {
		return fmt.Errorf("send steer to Claude SDK runner: %w", err)
	}
	t.steers = append(t.steers, ownedSteer{uuid: id, messageID: steer.MessageID})
	return nil
}

// openSteering lets t accept steers once its prompt is in the conversation.
func (d *SDKDriver) openSteering(t *sdkTurn) {
	d.steerMu.Lock()
	defer d.steerMu.Unlock()
	d.steering, t.awaitingInput = t, false
}

// ownedSteer is a steer message a turn accepted and the SDK message carrying it.
type ownedSteer struct {
	uuid, messageID string
	consumed        bool
}

// livePrompt reports whether the outcome waits on a prompt held by the running
// bridge. Such a turn keeps its subscription; a deferred call is parked on disk
// and releases it.
func livePrompt(outcome runtime.Outcome) bool {
	_, live := outcome.Pending.(*sdkPendingTurn)
	return live
}

// replay answers a deferred tool call. A fresh bridge resumes the conversation
// without a new message, Claude runs the call's PreToolUse hook again, and the
// approval broker returns the saved answer. The turn ends at the result that
// follows, which names no user message.
func (d *SDKDriver) replay(ctx context.Context, turn runtime.Turn, sink runtime.EventSink, tool *DeferredTool) (runtime.Outcome, error) {
	sub, err := d.session.BeginReplay(ctx)
	if err != nil {
		// Nothing was replayed, so the input stays answerable.
		return runtime.Outcome{}, &processStartError{err: fmt.Errorf("start Claude SDK runner for replay: %w", err)}
	}
	t := &sdkTurn{
		driver: d, sub: sub, turn: turn, seed: tool, budget: newActiveBudget(d.config.TurnTimeout),
		cleaning: d.replayCanceling(tool),
	}
	outcome, err := t.consume(ctx, sink)
	if err == nil && livePrompt(outcome) {
		// The replay is over and the turn now waits on a prompt the bridge
		// holds. Close the journal here: the pending turn that resolves the
		// prompt knows nothing of it, and an open entry would be read as an
		// abandoned replay by the next turn.
		if err := d.completeReplay(tool); err != nil {
			_ = outcome.Pending.Cancel(context.WithoutCancel(ctx), sink)
			return runtime.Outcome{}, err
		}
		return outcome, nil
	}
	d.session.EndTurn(sub)
	if err != nil && ctx.Err() != nil {
		var cancellationErr *runtime.CancellationError
		if !errors.As(err, &cancellationErr) {
			err = &runtime.CancellationError{Err: err}
		}
	}
	return outcome, err
}

type sdkTurn struct {
	driver *SDKDriver
	sub    *sdkrunner.Subscription
	// turn identifies the A2A task that owns a call deferred in this turn.
	turn runtime.Turn
	// uuid is the prompt this turn sent. A replay sends none.
	uuid string
	// seed is the deferred call a replay answers.
	seed *DeferredTool
	// cleaning is set while a canceled replay settles its transcript.
	cleaning bool
	// seedSettled is set once the replayed call has its result, after which
	// interrupting Claude leaves a consistent transcript.
	seedSettled    bool
	budget         *activeBudget
	parser         *parser
	sessionStarted bool
	pending        *sdkrunner.Perm
	// deferred is the call Claude deferred in this turn, with its session.
	deferred        *DeferredTool
	deferredSession string
	// interruptNow is set once a canceled replay's call has been refused.
	interruptNow bool

	// The fields below are guarded by driver.steerMu.
	// steers are the messages this turn accepted after its prompt.
	steers []ownedSteer
	// promptConsumed is set once a result has consumed the turn's prompt.
	promptConsumed bool
	// awaitingInput is set while the turn waits on a permission prompt.
	awaitingInput bool
}

// ownsResult reports whether a result ends this turn. A deferred result ends
// the A2A turn that is active: the call it leaves in the transcript needs a
// durable owner. A replay's result names no user message.
func (t *sdkTurn) ownsResult(raw json.RawMessage, uuids []string) bool {
	if sdkrunner.ResultDeferred(raw) {
		return true
	}
	if t.seed != nil {
		return len(uuids) == 0
	}
	if slices.Contains(uuids, t.uuid) {
		return true
	}
	t.driver.steerMu.Lock()
	defer t.driver.steerMu.Unlock()
	return slices.ContainsFunc(t.steers, func(s ownedSteer) bool { return slices.Contains(uuids, s.uuid) })
}

// consumeResult records the messages an owned result consumed. It reports
// whether the turn still owns a message no result has consumed; the turn then
// continues past this result. When it reports false, the turn stops accepting
// steers in the same critical section, so none can arrive after the decision.
func (t *sdkTurn) consumeResult(uuids []string) (unsettled bool) {
	t.driver.steerMu.Lock()
	defer t.driver.steerMu.Unlock()
	if t.seed == nil && !t.promptConsumed {
		t.promptConsumed = slices.Contains(uuids, t.uuid)
	}
	unsettled = t.seed == nil && !t.promptConsumed
	for i := range t.steers {
		if slices.Contains(uuids, t.steers[i].uuid) {
			t.steers[i].consumed = true
		}
		unsettled = unsettled || !t.steers[i].consumed
	}
	if !unsettled && t.driver.steering == t {
		t.driver.steering = nil
	}
	return unsettled
}

// closeSteering stops t accepting steers and returns those no result consumed.
func (t *sdkTurn) closeSteering() []string {
	t.driver.steerMu.Lock()
	defer t.driver.steerMu.Unlock()
	if t.driver.steering == t {
		t.driver.steering = nil
	}
	var unconsumed []string
	for _, s := range t.steers {
		if !s.consumed {
			unconsumed = append(unconsumed, s.messageID)
		}
	}
	return unconsumed
}

// settleSteering runs as Run or Resume returns. A turn waiting on a live
// permission prompt keeps its steers queued in the conversation, so it only
// pauses acceptance. Any other end reports the steers no result consumed: a
// deferred call, an exit, a cancellation or an exhausted budget leaves them
// unread, and the caller decides whether to send them again.
func (t *sdkTurn) settleSteering(waiting bool, sink runtime.EventSink) {
	if waiting {
		t.driver.steerMu.Lock()
		t.awaitingInput = true
		t.driver.steerMu.Unlock()
		return
	}
	if unconsumed := t.closeSteering(); len(unconsumed) != 0 {
		if steers, ok := sink.(runtime.SteerSink); ok {
			steers.SteersUnconsumed(unconsumed)
		}
	}
}

func (t *sdkTurn) consume(ctx context.Context, sink runtime.EventSink) (runtime.Outcome, error) {
	observer, observed := sink.(runtime.HealthSink)
	health := startHealthPublisher(observer)
	defer health.close(ctx)
	if t.parser == nil {
		t.parser = newParser(time.Now)
		if t.seed != nil {
			// The call was reported by the turn that deferred it.
			t.parser.tools[t.seed.ID] = t.seed.Name
			t.parser.emittedToolCalls[t.seed.ID] = struct{}{}
		}
		if observed {
			t.parser.health = newHealthProducer(t.driver.config.Workspace, t.driver.config.ExpectedVersion, t.seed)
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
			if ctx.Err() != nil && t.seed != nil && !t.cleaning {
				// A replay must settle its transcript before cancellation is
				// published, or the call would be replayed by the next turn. The
				// hooks now refuse it and every further tool; drain the result.
				t.cleaning = true
				if err := t.driver.cancelReplay(t.seed); err != nil {
					return runtime.Outcome{}, errors.Join(err, t.interrupt(context.WithoutCancel(ctx)))
				}
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancellationCleanupTimeout)
				defer cancel()
				ctx, sink = cleanupCtx, discardSink{ModelCallSink: sink}
				if t.seedSettled {
					// The call already ran; only Claude's answer to it remains.
					t.interruptReplay(ctx)
				}
				continue
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
			answered, err := t.answerFromJournal(ctx, item.Perm)
			if err != nil {
				return runtime.Outcome{}, err
			}
			if answered {
				continue
			}
			t.pending = item.Perm
			t.budget.pause()
			return runtime.Outcome{Pending: &sdkPendingTurn{turn: t}}, nil
		case sdkrunner.ItemPermDone:
			continue
		case sdkrunner.ItemSDK:
			outcome, done, err := t.translate(ctx, item.Raw, sink, health)
			if err != nil {
				return runtime.Outcome{}, err
			}
			if t.interruptNow {
				// The canceled call is refused; stop Claude from answering it.
				t.interruptNow = false
				t.interruptReplay(ctx)
			}
			if !done {
				continue
			}
			if t.deferred != nil {
				if t.cleaning {
					return runtime.Outcome{}, fmt.Errorf("Claude did not settle its canceled replay")
				}
				return t.park(ctx)
			}
			if t.cleaning {
				// Interrupting a refused call ends in an error result. The journal,
				// not this result, says whether the cancellation was consumed.
				return runtime.Outcome{}, nil
			}
			return outcome, nil
		}
	}
}

// interruptReplay stops Claude's work after a canceled replay's call has
// settled. A failure leaves the turn to end on its own result.
func (t *sdkTurn) interruptReplay(ctx context.Context) {
	if err := t.driver.session.Control(ctx, "interrupt", "", ""); err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "failed to interrupt a canceled claude replay", "error", err)
	}
}

// answerFromJournal answers a permission prompt for a tool inside the approval
// boundary, as the process driver's permission host does. Only the call whose
// saved answer the hook has just released is allowed. Any other protected call
// reached the prompt because Claude could not defer it, and is refused so that
// Claude retries it alone.
func (t *sdkTurn) answerFromJournal(ctx context.Context, perm *sdkrunner.Perm) (bool, error) {
	session := t.driver.session
	if t.cleaning {
		_, err := session.AnswerPerm(perm.ID, "deny", false, "The task was canceled.", sdkrunner.OriginA2A)
		return true, err
	}
	broker := t.driver.config.ApprovalBroker
	if broker == nil || !broker.protects(perm.ToolName) {
		return false, nil
	}
	output, err := broker.permissionDecision(&permissionPromptInput{ToolName: perm.ToolName, Input: perm.Input, ToolUseID: perm.ToolUseID})
	if err != nil {
		logging.FromContext(ctx).ErrorContext(ctx, "failed to resolve claude permission", "error", err)
		output = permissionPromptOutput{Behavior: "deny", Message: "Durable approval could not be resolved."}
	}
	if output.Behavior == "allow" {
		_, err = session.AllowPermWith(perm.ID, output.UpdatedInput, sdkrunner.OriginA2A)
	} else {
		_, err = session.AnswerPerm(perm.ID, "deny", false, output.Message, sdkrunner.OriginA2A)
	}
	return true, err
}

// park hands a deferred call to the durable journal. The bridge is stopped
// first: Claude replays the call when the conversation is next resumed, and
// that must happen under the turn that carries the answer.
func (t *sdkTurn) park(ctx context.Context) (runtime.Outcome, error) {
	tool, session := t.deferred, t.deferredSession
	t.deferred = nil
	if err := t.driver.session.StopAndWait(context.WithoutCancel(ctx)); err != nil {
		return runtime.Outcome{}, fmt.Errorf("stop Claude SDK runner at a deferred tool: %w", err)
	}
	return t.driver.parkDeferred(t.turn, session, tool, t.driver.replay)
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
	isResult, uuids := sdkrunner.ResultUUIDs(raw)
	if isResult && !t.ownsResult(raw, uuids) {
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
			t.deferred, t.deferredSession = event.Deferred, event.SessionID
			outcome = &runtime.Outcome{}
			return nil
		case EventToolActivity:
			if t.seed != nil && event.ToolPhase == "completed" && event.ToolID == t.seed.ID {
				t.seedSettled = true
				t.interruptNow = t.cleaning
			}
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
	if outcome == nil {
		return runtime.Outcome{}, false, nil
	}
	if isResult && t.consumeResult(uuids) && t.deferred == nil && !t.cleaning {
		// A steer is still queued behind this result. Its usage is reported;
		// the turn's outcome is that of the result that consumes the last one.
		return runtime.Outcome{}, false, nil
	}
	return *outcome, true, nil
}

// interrupt asks the SDK to stop the turn and waits up to the interrupt grace
// for its result. A bridge that does not settle is stopped, so the next demand
// starts a fresh bridge that resumes the conversation.
func (t *sdkTurn) interrupt(ctx context.Context) error {
	t.closeSteering()
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
			if isResult, uuids := sdkrunner.ResultUUIDs(item.Raw); isResult && t.ownsResult(item.Raw, uuids) {
				if sdkrunner.ResultDeferred(item.Raw) || !t.consumeResult(uuids) {
					return nil
				}
				// A queued steer starts its own SDK turn after this result.
				// Interrupt that one too, so it never runs without its task.
				if err := t.driver.session.Control(ctx, "interrupt", "", ""); err != nil {
					return t.driver.session.Stop()
				}
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
	t.driver.openSteering(t)
	outcome, err = t.consume(ctx, sink)
	owned = err == nil && livePrompt(outcome)
	t.settleSteering(owned, sink)
	return outcome, err
}

func (p *sdkPendingTurn) Cancel(ctx context.Context, sink runtime.ModelCallSink) error {
	defer p.turn.driver.session.EndTurn(p.turn.sub)
	err := p.turn.interrupt(ctx)
	if unconsumed := p.turn.closeSteering(); len(unconsumed) != 0 {
		if steers, ok := sink.(runtime.SteerSink); ok {
			steers.SteersUnconsumed(unconsumed)
		}
	}
	return err
}

var _ runtime.PendingTurn = (*sdkPendingTurn)(nil)
