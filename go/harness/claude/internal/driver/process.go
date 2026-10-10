// Package driver translates Claude Code's streaming process protocol into the
// runtime-neutral events consumed by the shared A2A executor.
package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"go.opentelemetry.io/otel/propagation"
)

// ProcessConfig contains validated, compiler-owned inputs for one Claude Code
// process. Actor-owned paths and environment are supplied by the adapter.
type ProcessConfig struct {
	Executable           string
	ExpectedVersion      string
	StrictVersion        bool
	Workspace            string
	Model                string
	AppendSystemPrompt   string
	AgentsJSON           string
	MCPConfigPath        string
	SettingsPath         string
	PermissionPromptTool string
	SkillRoot            string
	PluginDirs           []string
	DisallowedTools      []string
	Environment          []string
	MaxEventBytes        int
	MaxStderrBytes       int
	InterruptGrace       time.Duration
	PostResultGrace      time.Duration
	TurnTimeout          time.Duration
	ApprovalBroker       *ApprovalBroker
	// AwaitTelemetry holds each prompt until Claude Code telemetry has
	// initialized.
	AwaitTelemetry bool
}

// ProcessDriver supervises one Claude Code process per ordinary runtime turn
// and retains it while the permission-prompt MCP tool awaits a decision.
type ProcessDriver struct {
	config ProcessConfig
}

type parseItem struct {
	event *Event
	err   error
}

type processSession struct {
	command          *exec.Cmd
	items            <-chan parseItem
	stopEmit         chan struct{}
	wait             <-chan error
	stderr           *utils.BoundedBuffer
	lastResult       *runtime.Outcome
	sessionID        string
	stopOnce         sync.Once
	streamEnded      bool
	executionBudget  *activeBudget
	postResultBudget *activeBudget
	owner            *processTree
	stdout           io.ReadCloser
	stdin            io.WriteCloser
	stopErr          error
}

// pendingTurn owns a Claude process blocked in the permission MCP hook. Resume
// resolves the hook and continues that process; Cancel terminates it.
type pendingTurn struct {
	driver  *ProcessDriver
	session *processSession
	pending *PendingApprovalRequest
}

const interruptedResponseWarning = "API Error: Connection lost mid-response. The response above may be incomplete."

var errExecutionBudgetExceeded = errors.New("Claude execution budget exceeded")

// resumedEventSink removes Claude Code's synthetic connection warning after an
// intentional Actor pause. The live process and its provider stream are frozen
// while input is pending; Claude reports that interruption as assistant text
// when execution resumes even though it continues the tool call successfully.
// Keep the filter on the resume path so the same text remains visible if Claude
// emits it during an ordinary, uninterrupted turn.
type resumedEventSink struct {
	runtime.EventSink
}

func (s resumedEventSink) TextDelta(event runtime.TextDelta) error {
	if strings.TrimSpace(event.Text) == interruptedResponseWarning {
		return nil
	}
	return s.EventSink.TextDelta(event)
}

// NewProcessDriver constructs a Claude Code process driver.
func NewProcessDriver(config ProcessConfig) *ProcessDriver {
	if config.PostResultGrace <= 0 {
		config.PostResultGrace = claudeconfig.DefaultPostResultGrace
	}
	if config.TurnTimeout <= 0 {
		config.TurnTimeout = claudeconfig.DefaultTurnTimeout
	}
	return &ProcessDriver{config: config}
}

// Validate checks that the configured executable is the pinned Claude version.
func (d *ProcessDriver) Validate(ctx context.Context) error {
	path, err := exec.LookPath(d.config.Executable)
	if err != nil {
		return fmt.Errorf("find Claude executable %q: %w", d.config.Executable, err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir = d.config.Workspace
	cmd.Env = append([]string(nil), d.config.Environment...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("read Claude version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if d.config.StrictVersion && !strings.Contains(version, d.config.ExpectedVersion) {
		return fmt.Errorf("claude version mismatch: got %q, expected %q", version, d.config.ExpectedVersion)
	}
	return nil
}

// Args compiles one runtime turn into Claude Code command-line arguments.
func (d *ProcessDriver) Args(turn runtime.Turn) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--strict-mcp-config",
		"--dangerously-skip-permissions",
	}
	settings := d.config.SettingsPath
	if settings == "" {
		settings = defaultHarnessSettings
	}
	args = append(args, "--setting-sources", settingSources, "--settings", settings)
	if d.config.ApprovalBroker != nil {
		args = append(args, "--permission-prompt-tool", d.config.PermissionPromptTool)
	}
	if d.config.Model != "" {
		args = append(args, "--model", d.config.Model)
	}
	if len(d.config.DisallowedTools) != 0 {
		args = append(args, "--disallowedTools", strings.Join(d.config.DisallowedTools, ","))
	}
	if d.config.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", d.config.AppendSystemPrompt)
	}
	if d.config.AgentsJSON != "" {
		args = append(args, "--agents", d.config.AgentsJSON)
	}
	if d.config.MCPConfigPath != "" {
		args = append(args, "--mcp-config", d.config.MCPConfigPath)
	}
	if d.config.SkillRoot != "" {
		// --add-dir exposes compiler-selected skills materialized beneath
		// SkillRoot/.claude/skills.
		args = append(args, "--add-dir", d.config.SkillRoot)
	}
	for _, dir := range d.config.PluginDirs {
		args = append(args, "--plugin-dir", dir)
	}
	if turn.ContinuationID != "" {
		// Resume the Actor's exact root conversation. --continue selects Claude's
		// latest session and can be redirected by subagents or interrupted attempts.
		args = append(args, "--resume", turn.ContinuationID)
	}
	return args
}

// Run supervises one Claude Code process and emits its ordered runtime events.
func (d *ProcessDriver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (outcome runtime.Outcome, runErr error) {
	if strings.TrimSpace(turn.Prompt) == "" {
		return runtime.Outcome{}, fmt.Errorf("Claude prompt is required")
	}
	message, err := userMessage(turn.Prompt)
	if err != nil {
		return runtime.Outcome{}, err
	}
	environment := traceEnvironment(ctx, d.config.Environment)
	var gate *tracingGate
	if d.config.AwaitTelemetry {
		if gate, err = newTracingGate(); err != nil {
			warnTelemetryNotReady(ctx, "port_unavailable", err)
		} else {
			environment = gate.environment(environment)
		}
	}
	cmd := exec.Command(d.config.Executable, d.Args(turn)...)
	utils.ConfigureProcessGroup(cmd)
	cmd.Dir = d.config.Workspace
	cmd.Env = environment
	cmd.WaitDelay = d.config.InterruptGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtime.Outcome{}, fmt.Errorf("open Claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return runtime.Outcome{}, fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr := utils.NewBoundedBuffer(d.config.MaxStderrBytes)
	cmd.Stderr = stderr
	owner, err := newProcessTree()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return runtime.Outcome{}, err
	}
	if err := owner.start(cmd); err != nil {
		owner.release()
		_ = stdin.Close()
		_ = stdout.Close()
		return runtime.Outcome{}, fmt.Errorf("start Claude: %w", err)
	}
	items := make(chan parseItem)
	stopEmit := make(chan struct{})
	parseDone := make(chan struct{})
	go func() {
		defer close(items)
		parseErr := ParseJSONL(stdout, d.config.MaxEventBytes, func(event Event) error {
			select {
			case items <- parseItem{event: &event}:
				return nil
			case <-stopEmit:
				return context.Canceled
			}
		})
		// Wait closes pipes returned by StdoutPipe, so it must not run until the
		// parser has finished its final read.
		close(parseDone)
		select {
		case items <- parseItem{err: parseErr}:
		case <-stopEmit:
		}
	}()
	waitDone := make(chan error, 1)
	session := &processSession{
		command: cmd, items: items, stopEmit: stopEmit, wait: waitDone, stderr: stderr,
		executionBudget: newActiveBudget(d.config.TurnTimeout),
		owner:           owner, stdout: stdout, stdin: stdin,
	}
	go func() {
		<-parseDone
		waitDone <- session.command.Wait()
		close(waitDone)
	}()
	go sendPrompt(ctx, stdin, message, gate, parseDone)
	sessionOwnedByPendingTurn := false
	defer func() {
		if !sessionOwnedByPendingTurn {
			if cleanupErr := d.stopSession(session); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
	}()
	outcome, runErr = d.consume(ctx, session, sink)
	// A pending outcome carries this same live session. Every other return path
	// leaves cleanup with this Run invocation.
	sessionOwnedByPendingTurn = runErr == nil && outcome.Pending != nil
	return outcome, runErr
}

// traceEnvironment injects the trace context into the environment variables.
// Claude reads it once at startup, so a turn that is resumed after an approval
// keeps emitting under the trace of the request that started the process. The
// resumed A2A segment records a link to that origin rather than reparenting it.
func traceEnvironment(ctx context.Context, environment []string) []string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	result := make([]string, 0, len(environment)+2)
	// First remove the trace context variables from the environment.
	for _, variable := range environment {
		if strings.HasPrefix(variable, "TRACEPARENT=") || strings.HasPrefix(variable, "TRACESTATE=") {
			continue
		}
		result = append(result, variable)
	}
	if traceparent := carrier.Get("traceparent"); traceparent != "" {
		result = append(result, "TRACEPARENT="+traceparent)
	}
	if tracestate := carrier.Get("tracestate"); tracestate != "" {
		result = append(result, "TRACESTATE="+tracestate)
	}
	return result
}

// userMessage encodes a prompt as one Claude Code stream-JSON input line.
func userMessage(prompt string) ([]byte, error) {
	type content struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	message, err := json.Marshal(struct {
		Type    string  `json:"type"`
		Message content `json:"message"`
	}{Type: "user", Message: content{Role: "user", Content: prompt}})
	if err != nil {
		return nil, fmt.Errorf("encode Claude prompt: %w", err)
	}
	return append(message, '\n'), nil
}

// sendPrompt writes the prompt once the gate opens, then ends the input as a
// prompt argument would, since Claude holds its result for background agents
// only once its input has ended. A gate that times out still sends the
// prompt, trading the turn's native telemetry for the turn.
func sendPrompt(ctx context.Context, stdin io.WriteCloser, message []byte, gate *tracingGate, exited <-chan struct{}) {
	defer stdin.Close()
	if gate != nil {
		err := gate.wait(ctx, exited)
		if errors.Is(err, errProcessExited) || ctx.Err() != nil {
			return
		}
		if err != nil {
			warnTelemetryNotReady(ctx, "timeout", err)
		}
	}
	// A failed write means Claude has exited, which consume reports.
	_, _ = stdin.Write(message)
}

func (d *ProcessDriver) consume(ctx context.Context, session *processSession, sink runtime.EventSink) (runtime.Outcome, error) {
	var approvalPending *PendingApprovalRequest
	for {
		if err := ctx.Err(); err != nil {
			return runtime.Outcome{}, err
		}
		if approvalPending != nil && session.sessionID != "" {
			if !approvalPending.waiting() {
				approvalPending = nil
				session.executionBudget.resume()
				session.postResultBudget.resume()
				continue
			}
			return runtime.Outcome{Pending: &pendingTurn{
				driver: d, session: session, pending: approvalPending,
			}}, nil
		}
		if session.executionBudget.expired() {
			return runtime.Outcome{}, runtime.NewTerminalFailure("Claude execution budget exceeded (approval wait time excluded)", errExecutionBudgetExceeded)
		}
		if session.postResultBudget.expired() {
			logging.FromContext(ctx).WarnContext(ctx, "claude post-result grace expired; stopping remaining native work", "grace", d.config.PostResultGrace)
			return *session.lastResult, nil
		}
		var approvals <-chan *PendingApprovalRequest
		if d.config.ApprovalBroker != nil && approvalPending == nil {
			approvals = d.config.ApprovalBroker.Requests()
		}
		items := session.items
		var exited <-chan error
		if session.streamEnded {
			items = nil
			exited = session.wait
		}
		select {
		case request := <-approvals:
			// A result ends a Claude iteration, not necessarily this process.
			// Background completion can start another iteration with approvals.
			approvalPending = request
			session.executionBudget.pause()
			session.postResultBudget.pause()
		case item, ok := <-items:
			if !ok {
				return runtime.Outcome{}, fmt.Errorf("claude parser stopped without a result")
			}
			if item.event != nil {
				outcome, err := emitEvent(*item.event, sink)
				if err == nil {
					if item.event.Kind == EventSessionStarted {
						if session.sessionID != "" && session.sessionID != item.event.SessionID {
							return runtime.Outcome{}, fmt.Errorf("Claude changed session ID during an active process")
						}
						session.sessionID = item.event.SessionID
					}
					if outcome != nil {
						// The first result starts a finite allowance for any further
						// iterations. Later results do not reset that allowance.
						if session.postResultBudget == nil {
							session.postResultBudget = newActiveBudget(d.config.PostResultGrace)
							if approvalPending != nil {
								session.postResultBudget.pause()
							}
						}
						session.lastResult = outcome
					}
					continue
				}
				return runtime.Outcome{}, err
			}
			if item.err != nil {
				// A process that exits before its result can explain the failure
				// only on stderr. Reap it to finish draining stderr; malformed
				// output can also stop the parser while the process is still alive.
				if err := d.stopSession(session); err != nil {
					return runtime.Outcome{}, errors.Join(item.err, err)
				}
				if stderr := session.stderr.Diagnostic(); stderr != "" {
					return runtime.Outcome{}, fmt.Errorf("%w: %s", item.err, stderr)
				}
				return runtime.Outcome{}, item.err
			}
			session.streamEnded = true
		case waitErr := <-exited:
			if waitErr != nil {
				stderr := session.stderr.Diagnostic()
				if stderr != "" {
					logging.FromContext(ctx).WarnContext(ctx, "claude exited with an error", "error", waitErr, "stderr", stderr)
				}
				return runtime.Outcome{}, exitError(waitErr, session.lastResult, stderr)
			}
			if session.lastResult == nil {
				return runtime.Outcome{}, fmt.Errorf("claude process exited without a terminal result")
			}
			return *session.lastResult, nil
		case <-session.executionBudget.done():
			return runtime.Outcome{}, runtime.NewTerminalFailure("Claude execution budget exceeded (approval wait time excluded)", errExecutionBudgetExceeded)
		case <-session.postResultBudget.done():
			// Recheck priority and the remaining budget at the top of the loop.
			continue
		case <-ctx.Done():
			return runtime.Outcome{}, ctx.Err()
		}
	}
}

// exitError preserves a terminal failure for the shared executor while keeping
// stderr in diagnostics only. Unexpected exits retain the generic status.
func exitError(waitErr error, terminal *runtime.Outcome, stderr string) error {
	if terminal != nil && terminal.Failure != nil {
		return runtime.NewTerminalFailure(terminal.Failure.Message, waitErr)
	}
	message := utils.SafeDiagnostic(stderr)
	if message == "" {
		return fmt.Errorf("claude exited with an error: %w", waitErr)
	}
	return fmt.Errorf("claude exited with an error: %w: %s", waitErr, message)
}

func (p *pendingTurn) Request() runtime.InputRequest { return p.pending.approvalRequest() }

// Resume resolves the permission MCP call and continues consuming the same process
// until it completes, fails, or returns another PendingTurn.
func (p *pendingTurn) Resume(ctx context.Context, response runtime.InputResponse, sink runtime.EventSink) (outcome runtime.Outcome, runErr error) {
	sessionOwnedByPendingTurn := false
	defer func() {
		if !sessionOwnedByPendingTurn {
			if cleanupErr := p.driver.stopSession(p.session); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
	}()
	decision, ok := response.(*runtime.ApprovalDecision)
	if !ok {
		return runtime.Outcome{}, fmt.Errorf("Claude is waiting for a structured tool approval response")
	}
	if decision.ID != p.pending.request.ID {
		return runtime.Outcome{}, fmt.Errorf("tool approval response ID %q does not match pending ID %q", decision.ID, p.pending.request.ID)
	}
	if err := p.pending.resolve(*decision); err != nil {
		return runtime.Outcome{}, err
	}
	p.session.executionBudget.resume()
	p.session.postResultBudget.resume()
	outcome, runErr = p.driver.consume(ctx, p.session, resumedEventSink{EventSink: sink})
	sessionOwnedByPendingTurn = runErr == nil && outcome.Pending != nil
	return outcome, runErr
}

// Cancel denies the outstanding permission MCP call and reaps the Claude process.
func (p *pendingTurn) Cancel(_ context.Context) error {
	_ = p.pending.resolve(runtime.ApprovalDecision{
		ID: p.pending.request.ID, Approved: false, RejectionReason: "The task was canceled.",
	})
	return p.driver.stopSession(p.session)
}

// Close releases the Actor-local permission MCP listener. Pending process ownership is
// transferred to the runtime.PendingTurn returned by Run.
func (d *ProcessDriver) Close() error {
	if d.config.ApprovalBroker != nil {
		return d.config.ApprovalBroker.Close()
	}
	return nil
}

// emitEvent translates a Claude event to a runtime event and emits it to the
// provided event sink, which is then consumed by the shared A2A executor.
func emitEvent(event Event, sink runtime.EventSink) (*runtime.Outcome, error) {
	switch event.Kind {
	case EventSessionStarted:
		return nil, sink.SessionStarted(runtime.SessionStarted{ContinuationID: event.SessionID})
	case EventTextDelta:
		return nil, sink.TextDelta(runtime.TextDelta{Text: event.Text})
	case EventToolActivity:
		switch event.ToolPhase {
		case "started":
			return nil, sink.ToolCall(runtime.ToolCall{
				ID: event.ToolID, Name: event.ToolName, Arguments: event.Metadata,
			})
		case "completed":
			return nil, sink.ToolResult(runtime.ToolResult{
				ID: event.ToolID, Name: event.ToolName, Result: event.ToolResult, IsError: event.ToolError,
			})
		default:
			return nil, fmt.Errorf("claude tool activity has unsupported phase %q", event.ToolPhase)
		}
	case EventCompleted:
		return &runtime.Outcome{}, nil
	case EventFailed:
		return &runtime.Outcome{Failure: &runtime.Failure{Message: runtime.NewTerminalFailure(event.SafeMessage, nil).PublicMessage()}}, nil
	default:
		return nil, fmt.Errorf("unsupported Claude event kind %q", event.Kind)
	}
}

func (d *ProcessDriver) stopSession(session *processSession) error {
	session.stopOnce.Do(func() {
		session.executionBudget.pause()
		session.postResultBudget.pause()
		close(session.stopEmit)
		// Unblock the parser and prompt sender independently of descendants
		// holding inherited pipes. WaitDelay bounds stderr drain on cleanup.
		_ = session.stdin.Close()
		_ = session.stdout.Close()
		interruptErr := session.owner.interrupt()
		timer := time.NewTimer(d.config.InterruptGrace)
		defer timer.Stop()
		select {
		case <-session.wait:
		case <-timer.C:
		}
		killErr := session.owner.kill()
		// Finish exec.Cmd's wait before reaping adopted children, but keep a
		// failed signal or unresponsive leader from blocking this task forever.
		timer.Reset(d.config.InterruptGrace)
		select {
		case <-session.wait:
		case <-timer.C:
			session.stopErr = errors.Join(interruptErr, killErr, fmt.Errorf("Claude leader cleanup did not finish"))
			return
		}
		cleanupErr := session.owner.killAndReap()
		closeErr := session.owner.closeLeader()
		for range session.items {
		}
		session.stopErr = errors.Join(interruptErr, killErr, cleanupErr, closeErr)
		if session.stopErr == nil {
			session.owner.release()
		}
	})
	return session.stopErr
}

var _ runtime.PendingTurn = (*pendingTurn)(nil)
