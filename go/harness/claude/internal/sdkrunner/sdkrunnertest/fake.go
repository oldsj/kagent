// Package sdkrunnertest provides a scripted in-process stand-in for the Node
// SDK bridge.
package sdkrunnertest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
)

// Op is one operation the harness sent to the bridge.
type Op struct {
	Op       string          `json:"op"`
	Options  json.RawMessage `json:"options"`
	UUID     string          `json:"uuid"`
	Text     string          `json:"text"`
	Ref      string          `json:"ref"`
	ID       string          `json:"id"`
	Behavior string          `json:"behavior"`
	Model    string          `json:"model"`
	Effort   string          `json:"effort"`
	Mode     string          `json:"mode"`
}

// Bridge is a running fake bridge. Handler scripts replies.
type Bridge struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	once    sync.Once
	writeMu sync.Mutex
	exitErr error
	Spec    sdkrunner.Spec
}

// Emit writes one event line to the harness.
func (b *Bridge) Emit(event any) {
	line, _ := json.Marshal(event)
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, _ = b.stdoutW.Write(append(line, '\n'))
}

// SDK emits an SDK message.
func (b *Bridge) SDK(message map[string]any) { b.Emit(map[string]any{"k": "sdk", "m": message}) }

// Exit ends the bridge as if the process died.
func (b *Bridge) Exit() {
	b.once.Do(func() {
		_ = b.stdoutW.Close()
		_ = b.stdinR.Close()
		close(b.done)
	})
}

func (b *Bridge) Stdin() io.WriteCloser { return b.stdinW }
func (b *Bridge) Stdout() io.Reader     { return b.stdoutR }
func (b *Bridge) Pid() int              { return -1 }
func (b *Bridge) Wait() error {
	<-b.done
	return b.exitErr
}
func (b *Bridge) Stop(time.Duration) error {
	b.Exit()
	return nil
}

// Launcher launches fake bridges. Handle is called for every operation after
// start, from the bridge's reader goroutine.
type Launcher struct {
	Handle func(b *Bridge, op Op)

	mu      sync.Mutex
	bridges []*Bridge
	ops     []Op
	started chan *Bridge
}

// NewLauncher returns a launcher whose bridges call handle.
func NewLauncher(handle func(b *Bridge, op Op)) *Launcher {
	return &Launcher{Handle: handle, started: make(chan *Bridge, 16)}
}

func (l *Launcher) Launch(context.Context) (sdkrunner.Process, error) {
	b := &Bridge{done: make(chan struct{})}
	b.stdinR, b.stdinW = io.Pipe()
	b.stdoutR, b.stdoutW = io.Pipe()
	l.mu.Lock()
	l.bridges = append(l.bridges, b)
	l.mu.Unlock()
	go func() {
		scanner := bufio.NewScanner(b.stdinR)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			var op Op
			if json.Unmarshal(scanner.Bytes(), &op) != nil {
				continue
			}
			l.mu.Lock()
			l.ops = append(l.ops, op)
			l.mu.Unlock()
			if op.Op == "start" {
				_ = json.Unmarshal(op.Options, &b.Spec)
				l.started <- b
				continue
			}
			if l.Handle != nil {
				l.Handle(b, op)
			}
		}
	}()
	return b, nil
}

// Launches is how many bridges were started.
func (l *Launcher) Launches() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bridges)
}

// Started waits for the next bridge to receive its start operation.
func (l *Launcher) Started(timeout time.Duration) *Bridge {
	select {
	case b := <-l.started:
		return b
	case <-time.After(timeout):
		return nil
	}
}

// Ops returns every operation received after start, in order.
func (l *Launcher) Ops() []Op {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Op(nil), l.ops...)
}

// Continuation is an in-memory continuation store.
type Continuation struct {
	mu sync.Mutex
	ID string
}

func (c *Continuation) Load() (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ID, c.ID != "", nil
}

func (c *Continuation) Bind(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ID = id
	return nil
}

// Activity records busy/idle transitions.
type Activity struct {
	mu      sync.Mutex
	Reports []bool
}

func (a *Activity) SetActive(active bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Reports = append(a.Reports, active)
}

// Got returns the reports so far.
func (a *Activity) Got() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.Reports...)
}

// Echo is a Handle that answers each user message with an init (first time),
// assistant text and a result naming its uuid.
func Echo(sessionID string) func(*Bridge, Op) {
	var mu sync.Mutex
	initialized := map[*Bridge]bool{}
	return func(b *Bridge, op Op) {
		switch op.Op {
		case "user":
			mu.Lock()
			first := !initialized[b]
			initialized[b] = true
			mu.Unlock()
			if first {
				b.SDK(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID, "model": "claude-opus-5-5"})
			}
			b.SDK(map[string]any{"type": "assistant", "message": map[string]any{"id": "m-" + op.UUID, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "echo: " + op.Text}}}})
			b.SDK(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "echo: " + op.Text, "session_id": sessionID, "user_message_uuids": []string{op.UUID}})
		case "interrupt", "model", "effort", "mode":
			b.Emit(map[string]any{"k": "ack", "ref": op.Ref, "what": op.Op, "ms": 1, "model": op.Model, "effort": op.Effort, "mode": op.Mode})
		}
	}
}
