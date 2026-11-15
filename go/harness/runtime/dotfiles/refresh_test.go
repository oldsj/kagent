package dotfiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// Fictional: the owner's next commit rewrites AGENTS.md and drops the command.
var updated = []File{
	{Path: "AGENTS.md", Data: []byte("# Owner notes\nPrefer small, reviewed changes.\n")},
	{Path: "skills/release/SKILL.md", Data: []byte("---\nname: release\n---\nRelease carefully.\n")},
}

// switchSource is a delivery the test replaces, as UpdateSessionDotfiles does.
type switchSource struct {
	mu       sync.Mutex
	delivery *runtimev1.DotfilesDelivery
	err      error
}

func (s *switchSource) set(d *runtimev1.DotfilesDelivery, err error) {
	s.mu.Lock()
	s.delivery, s.err = d, err
	s.mu.Unlock()
}

func (s *switchSource) Dotfiles(context.Context) (*runtimev1.DotfilesDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivery, s.err
}

// events records the restarts and the turns in order.
type events struct {
	mu   sync.Mutex
	log  []string
	idle []bool // what successive restarts answer; true once exhausted
}

func (e *events) add(s string) {
	e.mu.Lock()
	e.log = append(e.log, s)
	e.mu.Unlock()
}

func (e *events) restart(context.Context) (bool, error) {
	e.add("restart")
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.idle) == 0 {
		return true, nil
	}
	idle := e.idle[0]
	e.idle = e.idle[1:]
	return idle, nil
}

func (e *events) Run(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
	e.add("turn")
	return runtime.Outcome{}, nil
}

func (e *events) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.log
	e.log = nil
	return out
}

func sameEvents(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func refreshLayer(t *testing.T) (*Layer, *events, *switchSource, string) {
	t.Helper()
	body := bundle(t, sample)
	data := dataDir(t)
	source := &switchSource{delivery: delivery(serve(t, body), body)}
	ev := &events{}
	l := New(ev, source, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state"))
	l.OnChange(ev.restart)
	return l, ev, source, filepath.Join(data, "claude")
}

// An updated Session is laid out at its next turn, and the agent is restarted
// before that turn so it reads the new files; turns after that restart nothing.
func TestAnUpdatedSessionIsLaidOutAtItsNextTurnAndTheAgentRestarted(t *testing.T) {
	l, ev, source, dir := refreshLayer(t)
	turn(t, l)
	if got := ev.take(); !sameEvents(got, "restart", "turn") {
		t.Fatalf("first turn = %v, want the first layout offered to the hook", got)
	}

	next := bundle(t, updated)
	source.set(delivery(serve(t, next), next), nil)
	turn(t, l)
	if got := ev.take(); !sameEvents(got, "restart", "turn") {
		t.Fatalf("turn after the update = %v", got)
	}
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != string(updated[0].Data) {
		t.Errorf("AGENTS.md = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "commands", "ship.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dropped command is still laid out: %v", err)
	}
	if got := mode(t, filepath.Join(dir, "AGENTS.md")); got != 0o444 {
		t.Errorf("mode = %v", got)
	}

	turn(t, l)
	if got := ev.take(); !sameEvents(got, "turn") {
		t.Fatalf("a later turn = %v, want no restart", got)
	}
}

// An agent that is busy outside the turn is not restarted under it: the
// restart stays pending and is tried again before each turn until it is done,
// or until the agent starts again by itself and reads the new files.
func TestTheRestartWaitsUntilTheAgentIsIdle(t *testing.T) {
	l, ev, source, _ := refreshLayer(t)
	turn(t, l)
	ev.take()
	ev.idle = []bool{false, false}

	next := bundle(t, updated)
	source.set(delivery(serve(t, next), next), nil)
	turn(t, l)
	turn(t, l)
	if got := ev.take(); !sameEvents(got, "restart", "turn", "restart", "turn") {
		t.Fatalf("turns while busy = %v", got)
	}
	turn(t, l)
	turn(t, l)
	if got := ev.take(); !sameEvents(got, "restart", "turn", "turn") {
		t.Fatalf("turns once idle = %v", got)
	}

	// An agent that starts again by itself (Ready, at its launch) settles it.
	other := bundle(t, sample)
	source.set(delivery(serve(t, other), other), nil)
	ev.idle = []bool{false}
	turn(t, l)
	l.Ready(context.Background())
	turn(t, l)
	if got := ev.take(); !sameEvents(got, "restart", "turn", "turn") {
		t.Fatalf("turns around a launch = %v", got)
	}
}

// An update that cannot be read or verified leaves the files already laid out,
// keeps restoring them, and restarts nothing.
func TestAFailedUpdateKeepsTheFilesLaidOut(t *testing.T) {
	l, ev, source, dir := refreshLayer(t)
	turn(t, l)
	ev.take()

	source.set(nil, errors.New("control plane unavailable"))
	if err := removeEntry(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	turn(t, l)
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != string(sample[0].Data) {
		t.Errorf("AGENTS.md = %q, want the laid-out bundle restored", got)
	}

	next := bundle(t, updated)
	bad := delivery(serve(t, next), next)
	bad.Digest = Digest([]byte("another bundle"))
	source.set(bad, nil)
	turn(t, l)
	if got := read(t, filepath.Join(dir, "AGENTS.md")); got != string(sample[0].Data) {
		t.Errorf("AGENTS.md = %q, want the bundle that verified", got)
	}
	if got := ev.take(); !sameEvents(got, "turn", "turn") {
		t.Fatalf("turns = %v, want no restart", got)
	}
}

// A Session created without dotfiles that is given some lays them out from
// its next turn, and an agent already running is restarted to read them.
func TestASessionGivenDotfilesLaterLaysThemOut(t *testing.T) {
	data := dataDir(t)
	source := &switchSource{}
	ev := &events{}
	l := New(ev, source, Claude(filepath.Join(data, "claude")), filepath.Join(data, "state"))
	l.OnChange(ev.restart)
	turn(t, l)
	body := bundle(t, sample)
	source.set(delivery(serve(t, body), body), nil)
	turn(t, l)
	if got := read(t, filepath.Join(data, "claude", "AGENTS.md")); got != string(sample[0].Data) {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if got := ev.take(); !sameEvents(got, "turn", "restart", "turn") {
		t.Fatalf("turns = %v", got)
	}
}
