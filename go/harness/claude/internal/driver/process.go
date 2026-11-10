// Package driver translates Claude Code's streaming process protocol into the
// runtime-neutral events consumed by the shared A2A executor.
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/propagation"

	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// ProcessConfig contains validated, compiler-owned inputs for one Claude Code
// process. Actor-owned paths and environment are supplied by the adapter.
type ProcessConfig struct {
	Executable             string
	ExpectedVersion        string
	StrictVersion          bool
	Workspace              string
	Model                  string
	AppendSystemPromptFile string
	AgentsJSON             string
	MCPConfigPath          string
	SettingsPath           string
	PermissionPromptTool   string
	PluginDirs             []string
	DisallowedTools        []string
	Environment            []string
	MaxEventBytes          int
	MaxStderrBytes         int
	InterruptGrace         time.Duration
	// PostResultGrace bounds further native activity after the first result.
	PostResultGrace time.Duration
	// TurnTimeout bounds one native process. Time spent waiting for a deferred
	// input is outside any process and is not counted.
	TurnTimeout    time.Duration
	ApprovalBroker *ApprovalBroker
	// AwaitTelemetry holds each prompt until Claude Code telemetry has
	// initialized.
	AwaitTelemetry bool
}

// ProcessDriver supervises one Claude Code process per ordinary runtime turn
// and exits at durable deferred-input boundaries.
type ProcessDriver struct {
	config ProcessConfig
}

type parseItem struct {
	event *Event
	err   error
}

type processSession struct {
	command  *exec.Cmd
	items    <-chan parseItem
	stopEmit chan struct{}
	wait     <-chan error
	stderr   *utils.BoundedBuffer
	// terminal is the latest iteration result. A follow-up iteration, such as
	// one started by a background task's completion, supersedes it.
	terminal    *runtime.Outcome
	sessionID   string
	deferred    *DeferredTool
	stopOnce    sync.Once
	stopErr     error
	canceled    bool
	seed        *DeferredTool
	streamEnded bool
	owner       *processTree
	stdout      io.ReadCloser
	// executionBudget bounds the process; postResultBudget starts at the first
	// result and bounds what follows it.
	executionBudget  *activeBudget
	postResultBudget *activeBudget
	// backgroundTasks counts background tasks Claude has not yet reported to
	// the conversation.
	backgroundTasks int
}

// processStartError proves that no native process ran for this attempt.
type processStartError struct{ err error }

func (e *processStartError) Error() string { return e.err.Error() }
func (e *processStartError) Unwrap() error { return e.err }

const cancellationCleanupTimeout = 30 * time.Second

var errExecutionBudgetExceeded = errors.New("Claude execution budget exceeded")

// NewProcessDriver constructs a Claude Code process driver.
func NewProcessDriver(cfg ProcessConfig) *ProcessDriver {
	if cfg.PostResultGrace <= 0 {
		cfg.PostResultGrace = config.DefaultPostResultGrace
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = config.DefaultTurnTimeout
	}
	return &ProcessDriver{config: cfg}
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
	if d.config.ApprovalBroker != nil {
		args = append(args,
			"--setting-sources", settingSources,
			"--settings", d.config.SettingsPath,
			"--permission-prompt-tool", d.config.PermissionPromptTool,
		)
	}
	if d.config.Workspace != "" {
		// With the adapter's CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD, this
		// loads the checkout's CLAUDE.md and .claude/rules. Claude also reads
		// the checkout's enabledPlugins and extraKnownMarketplaces, which can
		// enable plugins already installed in the harness-owned user scope.
		// The adapter supplies the checkout's skills as a plugin.
		args = append(args, "--add-dir", d.config.Workspace)
	}
	model := turn.Model
	if model == "" {
		model = d.config.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if turn.Effort != "" {
		args = append(args, "--effort", turn.Effort)
	}
	if len(d.config.DisallowedTools) != 0 {
		args = append(args, "--disallowedTools", strings.Join(d.config.DisallowedTools, ","))
	}
	if d.config.AppendSystemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", d.config.AppendSystemPromptFile)
	}
	if d.config.AgentsJSON != "" {
		args = append(args, "--agents", d.config.AgentsJSON)
	}
	if d.config.MCPConfigPath != "" {
		args = append(args, "--mcp-config", d.config.MCPConfigPath)
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
func (d *ProcessDriver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	return d.run(ctx, turn, sink, nil)
}

func (d *ProcessDriver) run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink, seed *DeferredTool) (outcome runtime.Outcome, returnErr error) {
	started := false
	defer func() {
		if !started && returnErr != nil {
			returnErr = &processStartError{err: returnErr}
		} else if seed != nil && ctx.Err() != nil && returnErr != nil {
			var cancellationErr *runtime.CancellationError
			if !errors.As(returnErr, &cancellationErr) {
				returnErr = &runtime.CancellationError{Err: returnErr}
			}
		}
	}()
	if strings.TrimSpace(turn.Prompt) == "" && seed == nil {
		return runtime.Outcome{}, fmt.Errorf("Claude prompt is required")
	}
	message, err := userMessage(turn.Prompt)
	if err != nil {
		return runtime.Outcome{}, err
	}
	if seed != nil {
		message = nil
	} // --resume replays deferred input without a new user turn.
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
	// A descendant that keeps stderr open must not hold Wait after the leader exits.
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
	// Read before Claude starts, since the process saves its own totals when it ends.
	startUsage, err := sessionUsage(d.config.Environment, turn.ContinuationID)
	if err != nil {
		logging.FromContext(ctx).WarnContext(ctx, "failed to read the Claude session usage, so this turn adds no tokens that only its totals show", "error", err)
	}
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
		return runtime.Outcome{}, err
	}
	started = true
	var health *healthProducer
	if _, observed := sink.(runtime.HealthSink); observed {
		health = newHealthProducer(d.config.Workspace, d.config.ExpectedVersion, seed)
	}
	items := make(chan parseItem)
	stopEmit := make(chan struct{})
	parseDone := make(chan struct{})
	go func() {
		defer close(items)
		parseErr := parseObservedJSONL(ctx, stdout, d.config.MaxEventBytes, startUsage, seed, health, func(event Event) error {
			select {
			case items <- parseItem{event: &event}:
				return nil
			case <-stopEmit:
				return context.Canceled
			}
		})
		// Wait closes pipes returned by StdoutPipe, so it runs only after the parser
		// returns; a read still in flight then fails and its goroutine exits.
		close(parseDone)
		select {
		case items <- parseItem{err: parseErr}:
		case <-stopEmit:
		}
	}()
	waitDone := make(chan error, 1)
	go func() {
		<-parseDone
		waitDone <- cmd.Wait()
		close(waitDone)
	}()
	session := &processSession{
		command: cmd, items: items, stopEmit: stopEmit, wait: waitDone, stderr: stderr, seed: seed,
		owner: owner, stdout: stdout, executionBudget: newActiveBudget(d.config.TurnTimeout),
	}
	go sendPrompt(ctx, stdin, message, gate, parseDone)
	outcome, err = d.consume(ctx, session, sink)
	// Reap the whole native tree before the outcome is published or parked.
	// A tree that cannot be reaped keeps its ownership slot and fails this turn.
	if cleanupErr := d.stopSession(session, sink); cleanupErr != nil {
		return runtime.Outcome{}, errors.Join(err, cleanupErr)
	}
	if err == nil && session.deferred != nil {
		return d.parkDeferred(turn, session.sessionID, session.deferred)
	}
	return outcome, err
}

// sessionUsage reads the session totals Claude Code saved when its last process ended. A resumed
// process starts its result.modelUsage from these, so they are this process's baseline.
func sessionUsage(environment []string, sessionID string) (map[string]modelUsage, error) {
	usage := map[string]modelUsage{}
	if sessionID == "" {
		return usage, nil
	}
	var configDir string
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, config.ClaudeConfigDirEnvName+"="); ok {
			configDir = value
		}
	}
	if configDir == "" {
		return nil, fmt.Errorf("%s is not set", config.ClaudeConfigDirEnvName)
	}
	files, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("found %d files for session %s, want 1", len(files), sessionID)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return nil, err
	}
	for line := range bytes.Lines(raw) {
		if !bytes.Contains(line, []byte(`"cost-state"`)) {
			continue
		}
		var state struct {
			Type       string                `json:"type"`
			ModelUsage map[string]modelUsage `json:"modelUsage"`
		}
		if json.Unmarshal(line, &state) == nil && state.Type == "cost-state" && state.ModelUsage != nil {
			usage = state.ModelUsage
		}
	}
	return usage, nil
}

// traceEnvironment injects the trace context into the environment variables.
// Claude reads it once at startup. A deferred resume starts a new native process
// under the current A2A invocation's trace.
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

func (d *ProcessDriver) consume(ctx context.Context, session *processSession, sink runtime.EventSink) (outcome runtime.Outcome, returnErr error) {
	cleaning := false
	defer func() {
		if cleaning && returnErr != nil {
			returnErr = &runtime.CancellationError{Err: returnErr}
		}
	}()
	// Health is observation only: it never fails, delays or blocks cancellation
	// of the turn it describes.
	observer, _ := sink.(runtime.HealthSink)
	health := startHealthPublisher(observer)
	defer func() { health.close(ctx) }()
	for {
		if session.executionBudget.expired() {
			return runtime.Outcome{}, runtime.NewTerminalFailure("Claude execution budget exceeded (approval wait time excluded)", errExecutionBudgetExceeded)
		}
		if session.postResultBudget.expired() {
			logging.FromContext(ctx).WarnContext(ctx, "claude post-result grace expired; stopping remaining native work", "grace", d.config.PostResultGrace)
			return *session.terminal, nil
		}
		items := session.items
		var exited <-chan error
		if session.streamEnded {
			// Keep the budgets and cancellation effective while the process exits.
			items, exited = nil, session.wait
		}
		select {
		case item, ok := <-items:
			if !ok {
				return runtime.Outcome{}, fmt.Errorf("claude parser stopped without a result")
			}
			if item.event != nil {
				switch item.event.Kind {
				case EventHealth:
					health.publish(item.event.Health)
					continue
				case EventBackgroundTasks:
					session.backgroundTasks = item.event.BackgroundTasks
					session.holdPostResultGrace()
					continue
				case EventDeferred:
					if session.terminal != nil || session.deferred != nil {
						return runtime.Outcome{}, fmt.Errorf("duplicate Claude deferred result")
					}
					if session.sessionID != "" && session.sessionID != item.event.SessionID {
						return runtime.Outcome{}, fmt.Errorf("Claude deferred result changed session ID")
					}
					session.sessionID = item.event.SessionID
					session.deferred = item.event.Deferred
					session.terminal = &runtime.Outcome{}
					continue
				}
				// A deferred result ends the process. An ordinary result ends one
				// iteration: background completion can start another.
				outcome, err := emitEvent(*item.event, sink, session.deferred != nil)
				if err != nil {
					return runtime.Outcome{}, err
				}
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
						session.holdPostResultGrace()
					}
					session.terminal = outcome
				}
				continue
			}
			if item.err != nil {
				// A process that exits before its result can explain the failure
				// only on stderr. Reap it to finish draining stderr; malformed
				// output can also stop the parser while the process is still alive.
				if err := d.stopSession(session, sink); err != nil {
					return runtime.Outcome{}, errors.Join(item.err, err)
				}
				if stderr := session.stderr.Diagnostic(); stderr != "" {
					return runtime.Outcome{}, fmt.Errorf("%w: %s", item.err, stderr)
				}
				return runtime.Outcome{}, item.err
			}
			session.streamEnded = true
		case waitErr := <-exited:
			// ErrWaitDelay means Claude exited successfully while a descendant
			// still held stderr. Stopping the session kills and reaps that tree.
			if waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay) {
				stderr := session.stderr.Diagnostic()
				if stderr != "" {
					logging.FromContext(ctx).WarnContext(ctx, "claude exited with an error", "error", waitErr, "stderr", stderr)
				}
				return runtime.Outcome{}, exitError(waitErr, session.terminal, stderr)
			}
			if session.terminal == nil {
				return runtime.Outcome{}, fmt.Errorf("claude process exited without a terminal result")
			}
			if cleaning && (session.deferred != nil || session.terminal.Failure != nil) {
				return runtime.Outcome{}, fmt.Errorf("Claude did not settle its canceled replay")
			}
			if session.backgroundTasks > 0 && session.terminal.Failure == nil && session.deferred == nil && !cleaning {
				// Fail closed: a result only stands for the work the model saw.
				// Claude kills background work at print-mode wind-down and its wait
				// ceiling, and may flush a held result after the kill.
				logging.FromContext(ctx).WarnContext(ctx, "claude exited with unreported background tasks", "tasks", session.backgroundTasks)
				return runtime.Outcome{Failure: &runtime.Failure{Message: backgroundTasksUnreportedMessage(session.backgroundTasks)}}, nil
			}
			return *session.terminal, nil
		case <-session.executionBudget.done():
			return runtime.Outcome{}, runtime.NewTerminalFailure("Claude execution budget exceeded (approval wait time excluded)", errExecutionBudgetExceeded)
		case <-session.postResultBudget.done():
			// Recheck the remaining budget at the top of the loop.
			continue
		case <-ctx.Done():
			session.canceled = true
			if session.seed == nil || cleaning {
				return runtime.Outcome{}, ctx.Err()
			}
			// A deferred replay must settle its transcript before cancellation is
			// published. Deny subsequent tools and drain this process; killing it
			// here would leave its journal reserved forever.
			cleaning = true
			if err := d.cancelReplay(session.seed); err != nil {
				return runtime.Outcome{}, err
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancellationCleanupTimeout)
			defer cancel()
			ctx, sink = cleanupCtx, discardSink{ModelCallSink: sink}
		}
	}
}

// holdPostResultGrace pauses the post-result grace while a background task is
// unreported. Claude feeds a task's outcome back as another iteration of this
// turn, so only the execution budget bounds the task and its follow-up
// iteration. The grace therefore expires only when every task has been
// reported, and never decides a turn's success.
func (s *processSession) holdPostResultGrace() {
	if s.backgroundTasks > 0 {
		s.postResultBudget.pause()
	} else {
		s.postResultBudget.resume()
	}
}

func backgroundTasksUnreportedMessage(tasks int) string {
	if tasks == 1 {
		return "Claude ended the turn before reporting the outcome of 1 background task"
	}
	return fmt.Sprintf("Claude ended the turn before reporting the outcome of %d background tasks", tasks)
}

// exitError preserves a terminal failure for the shared executor while keeping
// stderr in diagnostics only. Unexpected exits retain the generic status.
func exitError(waitErr error, terminal *runtime.Outcome, stderr string) error {
	if terminal != nil && terminal.Failure != nil {
		return runtime.NewTerminalFailure(terminal.Failure.Message, waitErr)
	}
	if stderr == "" {
		return fmt.Errorf("claude exited with an error: %w", waitErr)
	}
	return fmt.Errorf("claude exited with an error: %w: %s", waitErr, stderr)
}

// Close releases the ephemeral permission and hook listener.
func (d *ProcessDriver) Close() error {
	if d.config.ApprovalBroker != nil {
		return d.config.ApprovalBroker.Close()
	}
	return nil
}

// emitEvent translates a Claude event to a runtime event and emits it to the
// provided event sink, which is then consumed by the shared A2A executor.
func emitEvent(event Event, sink runtime.EventSink, deferred bool) (*runtime.Outcome, error) {
	if deferred {
		return nil, fmt.Errorf("claude emitted %s after its deferred result", event.Kind)
	}
	switch event.Kind {
	case EventSessionStarted:
		return nil, sink.SessionStarted(runtime.SessionStarted{ContinuationID: event.SessionID})
	case EventTextDelta:
		return nil, sink.TextDelta(runtime.TextDelta{Text: event.Text})
	case EventToolActivity:
		switch event.ToolPhase {
		case "started":
			call := runtime.ToolCall{ID: event.ToolID, Name: event.ToolName, Arguments: event.Metadata, ParentToolID: event.ParentToolID}
			call.MCPServer, call.MCPTool = splitMCPName(event.ToolName)
			return nil, sink.ToolCall(call)
		case "completed":
			return nil, sink.ToolResult(runtime.ToolResult{
				ID: event.ToolID, Name: event.ToolName, Result: event.ToolResult, IsError: event.ToolError,
			})
		default:
			return nil, fmt.Errorf("claude tool activity has unsupported phase %q", event.ToolPhase)
		}
	case EventModelCall:
		sink.ModelCall(*event.ModelCall)
		return nil, nil
	case EventCompleted:
		return &runtime.Outcome{}, nil
	case EventFailed:
		return &runtime.Outcome{Failure: &runtime.Failure{Message: runtime.NewTerminalFailure(event.SafeMessage, nil).PublicMessage()}}, nil
	default:
		return nil, fmt.Errorf("unsupported Claude event kind %q", event.Kind)
	}
}

// stopSession ends the native tree and reaps it, then drains the parser. It
// returns an error when the tree could not be proven gone; the ownership slot
// is then retained so no later turn starts over surviving processes.
func (d *ProcessDriver) stopSession(session *processSession, sink runtime.ModelCallSink) error {
	session.stopOnce.Do(func() {
		session.executionBudget.pause()
		session.postResultBudget.pause()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for item := range session.items {
				if item.event != nil && item.event.ModelCall != nil && sink != nil {
					call := *item.event.ModelCall
					call.Canceled = session.canceled && call.UsagePartial
					sink.ModelCall(call)
				}
			}
		}()
		interruptErr := session.owner.interrupt()
		timer := time.NewTimer(d.config.InterruptGrace)
		defer timer.Stop()
		select {
		case <-session.wait:
		case <-timer.C:
		}
		// The leader can exit on the interrupt while a descendant that ignores
		// it remains alive. Kill everything still in the tree.
		killErr := session.owner.kill()
		timer.Reset(d.config.InterruptGrace)
		select {
		case <-session.wait:
		case <-timer.C:
			// A survivor outside the census can hold stdout open. Unblock the
			// parser so Wait can run; WaitDelay bounds the stderr drain.
			_ = session.stdout.Close()
			timer.Reset(d.config.InterruptGrace)
			select {
			case <-session.wait:
			case <-timer.C:
				close(session.stopEmit)
				session.stopErr = errors.Join(interruptErr, killErr, fmt.Errorf("Claude leader cleanup did not finish"))
				return
			}
		}
		// exec.Cmd has reaped the leader, so remaining children are adopted orphans.
		cleanupErr := session.owner.killAndReap()
		closeErr := session.owner.closeLeader()
		<-drained
		close(session.stopEmit)
		session.stopErr = errors.Join(interruptErr, killErr, cleanupErr, closeErr)
		if session.stopErr == nil {
			session.owner.release()
		}
	})
	return session.stopErr
}

// splitMCPName splits Claude's mcp__<server>__<tool> name at the first "__". Agent server
// names come from Kubernetes names, which never contain "__"; plugin servers are best effort.
func splitMCPName(name string) (server, tool string) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", ""
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return "", ""
	}
	return server, tool
}
