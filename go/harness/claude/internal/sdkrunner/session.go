package sdkrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Origin says who sent a user message into the conversation.
type Origin string

const (
	OriginClient Origin = "client"
	OriginA2A    Origin = "a2a"
)

// Runner states reported to clients.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateReady    = "ready"
	StateExited   = "exited"
)

// maxEventBytes bounds one bridge event line. SDK messages carry tool results,
// so the bound matches the process driver's default event limit.
const maxEventBytes = 16 << 20

// Continuation is the Actor's native conversation store.
type Continuation interface {
	Load() (string, bool, error)
	Bind(string) error
}

type settler interface {
	Settle(context.Context) error
}

// ActivityReporter receives busy/idle transitions. It is called only on a
// change, never at boot: the first busy state needs a client or a turn.
type ActivityReporter interface {
	SetActive(bool)
}

// Config configures a Session.
type Config struct {
	Launcher Launcher
	// Spec compiles the bridge start operation for a profile. It is called on
	// every bridge start, so a reconfigured harness applies on the next start.
	Spec func(Profile) (Spec, error)
	// Prepare runs before every bridge start (Git preparation). Nil skips it.
	Prepare      func(context.Context) error
	Continuation Continuation
	Activity     ActivityReporter
	// Dir is the runner's durable directory, normally /data/sdk-runner.
	Dir       string
	StopGrace time.Duration
	Logger    *slog.Logger
	// Backlog bounds the in-memory event backlog.
	Backlog int
	// ClientGate refuses a client's user message with the returned error. The
	// conversation takes no new message while a deferred tool call waits for
	// its answer: Claude would answer the message and drop the call. Nil
	// admits every message.
	ClientGate func() error
}

// Meta is the session metadata sent to clients.
type Meta struct {
	SessionID      string          `json:"sessionId,omitempty"`
	Model          string          `json:"model,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	Version        string          `json:"version,omitempty"`
	Commands       json.RawMessage `json:"commands,omitempty"`
	SlashCommands  json.RawMessage `json:"slash_commands,omitempty"`
	Models         json.RawMessage `json:"models,omitempty"`
	Account        json.RawMessage `json:"account,omitempty"`
	Profile        Profile         `json:"profile"`
}

// Perm is one permission request awaiting an answer.
type Perm struct {
	ID             string          `json:"id"`
	ToolName       string          `json:"toolName"`
	Input          map[string]any  `json:"input"`
	Title          string          `json:"title,omitempty"`
	Description    string          `json:"description,omitempty"`
	DecisionReason string          `json:"decisionReason,omitempty"`
	BlockedPath    string          `json:"blockedPath,omitempty"`
	Suggestions    json.RawMessage `json:"suggestions,omitempty"`
	ToolUseID      string          `json:"toolUseID,omitempty"`
}

// Session owns the Actor's one SDK conversation.
type Session struct {
	cfg Config

	// startMu serializes bridge starts, which may run Git preparation.
	startMu sync.Mutex
	// writeMu orders operations on the bridge's stdin.
	writeMu sync.Mutex
	// activityMu orders activity reports.
	activityMu sync.Mutex

	mu   sync.Mutex
	proc Process
	// procDone is closed once the reader has settled proc's exit.
	procDone  chan struct{}
	state     string
	meta      Meta
	overrides overrides
	perms     map[string]*Perm
	permOrder []string
	// outstanding maps each unconsumed user message to the A2A message ID it
	// carries; a client's own message carries none.
	outstanding map[string]string
	sdkState    string
	background  int
	turn        *Subscription
	clients     map[*client]struct{}
	seq         int64
	backlog     []entry
	busy        bool
	closed      bool
}

type overrides struct{ model, effort, mode string }

type entry struct {
	seq    int64
	stream bool
	data   json.RawMessage
}

// New constructs a Session. It starts nothing.
func New(cfg Config) (*Session, error) {
	if cfg.Launcher == nil || cfg.Spec == nil || cfg.Continuation == nil || cfg.Dir == "" {
		return nil, fmt.Errorf("sdk runner session requires a launcher, spec, continuation and directory")
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Backlog <= 0 {
		cfg.Backlog = 4000
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("prepare sdk runner directory: %w", err)
	}
	s := &Session{
		cfg: cfg, state: StateStopped, perms: map[string]*Perm{}, outstanding: map[string]string{},
		clients: map[*client]struct{}{},
	}
	s.meta.Profile = s.profile()
	return s, nil
}

// HomeDir is where a bootstrap config bundle is unpacked. Its presence
// selects the user profile.
func (s *Session) HomeDir() string { return filepath.Join(s.cfg.Dir, "home") }

func (s *Session) profile() Profile {
	if info, err := os.Stat(s.HomeDir()); err == nil && info.IsDir() {
		return ProfileUser
	}
	return ProfileManaged
}

// Start starts the bridge if it is not running.
func (s *Session) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	return s.startLocked(ctx)
}

// startLocked requires startMu.
func (s *Session) startLocked(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("sdk runner is closed")
	}
	if s.proc != nil {
		s.mu.Unlock()
		return nil
	}
	s.setStateLocked(StateStarting, nil)
	s.mu.Unlock()
	proc, err := s.launch(ctx)
	if err != nil {
		s.mu.Lock()
		s.setStateLocked(StateStopped, nil)
		s.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.proc, s.procDone = proc, done
	s.mu.Unlock()
	go s.read(proc, done)
	return nil
}

func (s *Session) launch(ctx context.Context) (Process, error) {
	if s.cfg.Prepare != nil {
		if err := s.cfg.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	if settle, ok := s.cfg.Continuation.(settler); ok {
		if err := settle.Settle(ctx); err != nil {
			return nil, fmt.Errorf("settle continuation: %w", err)
		}
	}
	resume, _, err := s.cfg.Continuation.Load()
	if err != nil {
		return nil, fmt.Errorf("load continuation: %w", err)
	}
	profile := s.profile()
	spec, err := s.cfg.Spec(profile)
	if err != nil {
		return nil, err
	}
	spec.Resume = resume
	s.mu.Lock()
	if s.overrides.model != "" {
		spec.Model = s.overrides.model
	}
	if s.overrides.effort != "" {
		spec.Effort = s.overrides.effort
	}
	if s.overrides.mode != "" {
		spec.PermissionMode = s.overrides.mode
	}
	s.meta.Profile = profile
	s.mu.Unlock()
	if err := spec.validate(); err != nil {
		return nil, err
	}
	proc, err := s.cfg.Launcher.Launch(ctx)
	if err != nil {
		return nil, fmt.Errorf("start sdk runner: %w", err)
	}
	if err := writeLine(proc.Stdin(), map[string]any{"op": "start", "options": spec}); err != nil {
		_ = proc.Stop(s.cfg.StopGrace)
		return nil, fmt.Errorf("start sdk runner session: %w", err)
	}
	s.cfg.Logger.InfoContext(ctx, "sdk runner started", "profile", profile, "resume", resume != "")
	return proc, nil
}

// Stop stops the bridge, if running. The next demand starts it again.
func (s *Session) Stop() error {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return nil
	}
	return proc.Stop(s.cfg.StopGrace)
}

// Close stops the bridge and refuses later starts.
func (s *Session) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.Stop()
}

// StopAndWait stops the bridge and returns once its exit has been settled, so
// no event of that bridge reaches a turn registered afterwards.
func (s *Session) StopAndWait(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	return s.stopAndWaitLocked(ctx)
}

// stopAndWaitLocked requires startMu, which keeps another start out.
func (s *Session) stopAndWaitLocked(ctx context.Context) error {
	s.mu.Lock()
	proc, done := s.proc, s.procDone
	s.mu.Unlock()
	if proc == nil {
		return nil
	}
	if err := proc.Stop(s.cfg.StopGrace); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BeginReplay registers the A2A turn on a fresh bridge that resumes the bound
// conversation without a new user message. Claude replays a deferred tool call
// when its conversation is resumed, so the turn must be registered before that
// bridge starts and no other start may come between.
func (s *Session) BeginReplay(ctx context.Context) (*Subscription, error) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if err := s.stopAndWaitLocked(ctx); err != nil {
		return nil, fmt.Errorf("stop sdk runner before replay: %w", err)
	}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("another A2A turn is active")
	}
	sub := newSubscription()
	s.turn = sub
	s.mu.Unlock()
	s.updateActivity()
	if err := s.startLocked(ctx); err != nil {
		s.EndTurn(sub)
		return nil, err
	}
	return sub, nil
}

// TurnActive reports whether an A2A turn owns the conversation's events.
func (s *Session) TurnActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn != nil
}

// SubmitFromClient sends a client's user message unless the client gate
// refuses it.
func (s *Session) SubmitFromClient(ctx context.Context, text, uuid string) error {
	if s.cfg.ClientGate != nil {
		if err := s.cfg.ClientGate(); err != nil {
			return err
		}
	}
	return s.Submit(ctx, text, uuid, OriginClient)
}

// Submit sends a user message into the conversation, starting the bridge.
func (s *Session) Submit(ctx context.Context, text, uuid string, origin Origin) error {
	return s.SubmitMessage(ctx, text, uuid, "", origin)
}

// SubmitMessage sends a user message that carries the A2A message messageID.
// Clients see messageID as mid on the message's echo and on the turn frame of
// the result that consumed it, so they can match a ledger entry to its turn.
func (s *Session) SubmitMessage(ctx context.Context, text, uuid, messageID string, origin Origin) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.outstanding[uuid] = messageID
	fields := map[string]any{"uuid": uuid, "text": text, "origin": origin}
	if messageID != "" {
		fields["mid"] = messageID
	}
	s.emitLocked("user", fields)
	s.mu.Unlock()
	s.updateActivity()
	if err := s.write(map[string]any{"op": "user", "uuid": uuid, "text": text}); err != nil {
		s.mu.Lock()
		delete(s.outstanding, uuid)
		s.mu.Unlock()
		s.updateActivity()
		return err
	}
	return nil
}

// Control applies interrupt, model, effort or mode. value is ignored for
// interrupt. An interrupt with no bridge running is a no-op.
func (s *Session) Control(ctx context.Context, what, value, ref string) error {
	op := map[string]any{"op": what, "ref": ref}
	switch what {
	case "interrupt":
		s.mu.Lock()
		running := s.proc != nil
		s.mu.Unlock()
		if !running {
			return nil
		}
		return s.write(op)
	case "model", "effort", "mode":
		op[what] = value
	default:
		return fmt.Errorf("unknown control %q", what)
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	return s.write(op)
}

// AnswerPerm resolves a pending permission request. It reports false when the
// request is no longer pending, for example because another party answered.
func (s *Session) AnswerPerm(id, behavior string, always bool, message string, by Origin) (bool, error) {
	return s.answerPerm(id, behavior, always, message, nil, by)
}

// AllowPermWith allows a pending permission request with updatedInput in place
// of the input Claude asked about. An AskUserQuestion call takes its answers
// this way.
func (s *Session) AllowPermWith(id string, updatedInput map[string]any, by Origin) (bool, error) {
	return s.answerPerm(id, "allow", false, "", updatedInput, by)
}

func (s *Session) answerPerm(id, behavior string, always bool, message string, updatedInput map[string]any, by Origin) (bool, error) {
	if behavior != "allow" && behavior != "deny" {
		return false, fmt.Errorf("permission behavior must be allow or deny")
	}
	s.mu.Lock()
	if _, ok := s.perms[id]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	s.removePermLocked(id)
	s.emitLocked("perm_done", map[string]any{"id": id, "behavior": behavior, "always": always, "by": by})
	if s.turn != nil && by != OriginA2A {
		s.turn.push(Item{Kind: ItemPermDone, PermID: id})
	}
	s.mu.Unlock()
	s.updateActivity()
	op := map[string]any{"op": "perm", "id": id, "behavior": behavior, "always": always, "message": message}
	if updatedInput != nil {
		op["updatedInput"] = updatedInput
	}
	return true, s.write(op)
}

// SessionID is the bound native session, once known.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta.SessionID
}

// BeginTurn registers the one A2A turn that receives conversation events.
func (s *Session) BeginTurn() (*Subscription, error) {
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, errors.New("another A2A turn is active")
	}
	sub := newSubscription()
	s.turn = sub
	s.mu.Unlock()
	s.updateActivity()
	return sub, nil
}

// EndTurn releases the A2A turn.
func (s *Session) EndTurn(sub *Subscription) {
	s.mu.Lock()
	if s.turn == sub {
		s.turn = nil
	}
	s.mu.Unlock()
	s.updateActivity()
}

func (s *Session) write(op any) error {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return errors.New("sdk runner is not running")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeLine(proc.Stdin(), op)
}

func writeLine(w interface{ Write([]byte) (int, error) }, value any) error {
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// read consumes one bridge's events until it exits.
func (s *Session) read(proc Process, done chan struct{}) {
	defer close(done)
	reader := bufio.NewReaderSize(proc.Stdout(), 64*1024)
	var readErr error
	for {
		line, err := readLine(reader, maxEventBytes)
		if len(line) > 0 {
			s.handle(line)
		}
		if err != nil {
			readErr = err
			break
		}
	}
	waitErr := proc.Wait()
	code := 0
	var exit interface{ ExitCode() int }
	if errors.As(waitErr, &exit) {
		code = exit.ExitCode()
	}
	s.cfg.Logger.Warn("sdk runner exited", "read", readErr, "wait", waitErr)
	s.mu.Lock()
	if s.proc == proc {
		s.proc = nil
	}
	for _, id := range append([]string(nil), s.permOrder...) {
		s.removePermLocked(id)
		s.emitLocked("perm_done", map[string]any{"id": id, "aborted": true})
	}
	clear(s.outstanding)
	s.sdkState, s.background = "", 0
	if s.turn != nil {
		s.turn.push(Item{Kind: ItemExited})
	}
	s.setStateLocked(StateExited, map[string]any{"code": code})
	s.mu.Unlock()
	s.updateActivity()
}

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			// Drop the oversized event rather than the bridge.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return nil, err
		}
		line = append(line, fragment...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

type bridgeEvent struct {
	K        string          `json:"k"`
	M        json.RawMessage `json:"m"`
	Ref      string          `json:"ref"`
	What     string          `json:"what"`
	MS       int64           `json:"ms"`
	Error    string          `json:"error"`
	Model    string          `json:"model"`
	Effort   string          `json:"effort"`
	Mode     string          `json:"mode"`
	ID       string          `json:"id"`
	Commands json.RawMessage `json:"commands"`
	Models   json.RawMessage `json:"models"`
	Account  json.RawMessage `json:"account"`
}

type sdkEnvelope struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	SessionID         string          `json:"session_id"`
	Model             string          `json:"model"`
	PermissionMode    string          `json:"permissionMode"`
	Cwd               string          `json:"cwd"`
	Version           string          `json:"claude_code_version"`
	State             string          `json:"state"`
	StopReason        string          `json:"stop_reason"`
	UserMessageUUID   string          `json:"user_message_uuid"`
	UserMessageUUIDs  []string        `json:"user_message_uuids"`
	SlashCommandsJSON json.RawMessage `json:"slash_commands"`
	Tasks             []struct {
		Ambient bool `json:"ambient"`
	} `json:"tasks"`
}

// ResultUUIDs returns the client user-message UUIDs a result consumed.
func ResultUUIDs(m json.RawMessage) (isResult bool, uuids []string) {
	var envelope sdkEnvelope
	if json.Unmarshal(m, &envelope) != nil || envelope.Type != "result" {
		return false, nil
	}
	uuids = envelope.UserMessageUUIDs
	if len(uuids) == 0 && envelope.UserMessageUUID != "" {
		uuids = []string{envelope.UserMessageUUID}
	}
	return true, uuids
}

// ResultDeferred reports whether m is the result that ends a turn at a
// deferred tool call. Claude keeps the call in its transcript and replays it
// when the conversation is next resumed.
func ResultDeferred(m json.RawMessage) bool {
	var envelope sdkEnvelope
	return json.Unmarshal(m, &envelope) == nil && envelope.Type == "result" && envelope.StopReason == "tool_deferred"
}

func (s *Session) handle(line []byte) {
	var event bridgeEvent
	if err := json.Unmarshal(line, &event); err != nil {
		s.cfg.Logger.Warn("sdk runner sent a malformed event", "error", err)
		return
	}
	s.mu.Lock()
	switch event.K {
	case "sdk":
		s.handleSDKLocked(event.M)
	case "init":
		s.meta.Commands, s.meta.Models, s.meta.Account = event.Commands, event.Models, event.Account
		s.setStateLocked(StateReady, nil)
		s.emitLocked("meta", map[string]any{"meta": s.meta})
	case "perm":
		var perm Perm
		if err := json.Unmarshal(line, &perm); err != nil || perm.ID == "" {
			s.cfg.Logger.Warn("sdk runner sent a malformed permission request")
			break
		}
		s.perms[perm.ID] = &perm
		s.permOrder = append(s.permOrder, perm.ID)
		s.emitLocked("perm", permFields(&perm))
		if s.turn != nil {
			s.turn.push(Item{Kind: ItemPerm, Perm: &perm})
		}
	case "perm_aborted":
		if _, ok := s.perms[event.ID]; ok {
			s.removePermLocked(event.ID)
			s.emitLocked("perm_done", map[string]any{"id": event.ID, "aborted": true})
			if s.turn != nil {
				s.turn.push(Item{Kind: ItemPermDone, PermID: event.ID})
			}
		}
	case "ack":
		if event.Error != "" {
			s.emitLocked("err", map[string]any{"text": event.Error, "ref": event.Ref})
			break
		}
		fields := map[string]any{"what": event.What, "ms": event.MS}
		switch event.What {
		case "model":
			s.overrides.model, s.meta.Model, fields["model"] = event.Model, event.Model, event.Model
		case "effort":
			s.overrides.effort, s.meta.Effort, fields["effort"] = event.Effort, event.Effort, event.Effort
		case "mode":
			s.overrides.mode, s.meta.PermissionMode, fields["mode"] = event.Mode, event.Mode, event.Mode
		}
		s.emitLocked("ctl", fields)
		if event.What != "interrupt" {
			s.emitLocked("meta", map[string]any{"meta": s.meta})
		}
	case "ended":
		if event.Error != "" {
			s.cfg.Logger.Warn("sdk runner query ended", "error", event.Error)
			s.emitLocked("err", map[string]any{"text": "The Claude session ended: " + event.Error})
		}
	}
	s.mu.Unlock()
	s.updateActivity()
}

func (s *Session) handleSDKLocked(m json.RawMessage) {
	var envelope sdkEnvelope
	if err := json.Unmarshal(m, &envelope); err != nil {
		return
	}
	switch {
	case envelope.Type == "system" && envelope.Subtype == "init":
		s.meta.SessionID, s.meta.Model, s.meta.PermissionMode = envelope.SessionID, envelope.Model, envelope.PermissionMode
		s.meta.Cwd, s.meta.Version, s.meta.SlashCommands = envelope.Cwd, envelope.Version, envelope.SlashCommandsJSON
		if envelope.SessionID != "" {
			if err := s.cfg.Continuation.Bind(envelope.SessionID); err != nil {
				s.cfg.Logger.Error("bind sdk runner continuation", "error", err)
			}
		}
		s.setStateLocked(StateReady, nil)
		s.emitLocked("meta", map[string]any{"meta": s.meta})
	case envelope.Type == "system" && envelope.Subtype == "status" && envelope.PermissionMode != "":
		s.meta.PermissionMode = envelope.PermissionMode
	case envelope.Type == "system" && envelope.Subtype == "session_state_changed":
		s.sdkState = envelope.State
	case envelope.Type == "system" && envelope.Subtype == "background_tasks_changed":
		s.background = 0
		for _, task := range envelope.Tasks {
			if !task.Ambient {
				s.background++
			}
		}
	case envelope.Type == "assistant":
		// The full message supersedes its partial stream in the backlog.
		for len(s.backlog) > 0 && s.backlog[len(s.backlog)-1].stream {
			s.backlog = s.backlog[:len(s.backlog)-1]
		}
	}
	s.emitStreamLocked("sdk", map[string]any{"m": m}, envelope.Type == "stream_event")
	if envelope.Type == "result" {
		_, uuids := ResultUUIDs(m)
		mids := []string{}
		for _, id := range uuids {
			if mid := s.outstanding[id]; mid != "" {
				mids = append(mids, mid)
			}
			delete(s.outstanding, id)
		}
		state := "idle"
		if len(s.outstanding) != 0 {
			state = "running"
		}
		s.emitLocked("turn", map[string]any{"state": state, "uuids": uuids, "mids": mids})
	}
	if s.turn != nil {
		s.turn.push(Item{Kind: ItemSDK, Raw: m})
	}
}

func permFields(perm *Perm) map[string]any {
	raw, _ := json.Marshal(perm)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	return fields
}

func (s *Session) removePermLocked(id string) {
	delete(s.perms, id)
	for i, existing := range s.permOrder {
		if existing == id {
			s.permOrder = append(s.permOrder[:i], s.permOrder[i+1:]...)
			break
		}
	}
}

func (s *Session) setStateLocked(state string, extra map[string]any) {
	if s.state == state && extra == nil {
		return
	}
	s.state = state
	fields := map[string]any{"state": state}
	maps.Copy(fields, extra)
	s.emitLocked("runner", fields)
}

// busyLocked is the activity predicate from the contract. SDK work that is
// blocked on a prompt for a human (a pending permission or question, or SDK
// state requires_action) is not activity by itself: with no client attached
// and no A2A turn, the question is parked at a durable boundary, and counting
// it would hold the Actor awake until someone answers. An attached client, a
// running A2A turn and background tasks still count.
func (s *Session) busyLocked() bool {
	waitingForHuman := len(s.perms) > 0 || s.sdkState == "requires_action"
	working := !waitingForHuman && (len(s.outstanding) > 0 || s.sdkState == "running")
	return len(s.clients) > 0 || s.turn != nil || working || s.background > 0
}

// updateActivity reports a busy/idle change.
func (s *Session) updateActivity() {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	s.mu.Lock()
	busy := s.busyLocked()
	changed := busy != s.busy
	s.busy = busy
	s.mu.Unlock()
	if changed && s.cfg.Activity != nil {
		s.cfg.Activity.SetActive(busy)
	}
}

// Busy reports the current activity state.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLocked()
}
