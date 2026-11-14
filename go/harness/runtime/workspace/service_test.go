package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime/service"
)

// The test binary stands in for the harness binary: a workspace service
// re-executes it in supervisor mode.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == service.SupervisorArg {
		os.Exit(service.Supervise(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestServeService(t *testing.T) {
	t.Run("Given a Session that declares a service and has its checkout, when the runtime serves the Session, assert that the service runs in the checkout", func(t *testing.T) {
		source := &sessionSource{request: &Request{Repo: "https://github.com/o/r", ServiceCommand: []string{"/bin/sh", "-c", "pwd; exec /bin/sleep 60"}}}
		source.serve()
		b, dir := checkedOut(t, source)

		b.ServeService(serving(t), ServiceConfig{Dir: dir, Environment: os.Environ(), Executable: os.Args[0]})

		status := waitForService(t, dir, "running")
		if status.Command[0] != "/bin/sh" || status.PID == 0 {
			t.Fatalf("status = %+v, want the declared command running", status)
		}
		checkout, err := filepath.EvalSymlinks(b.dir)
		if err != nil {
			t.Fatal(err)
		}
		waitForLog(t, dir, checkout)
	})

	t.Run("Given a process restored before it serves a Session, when the Session identity appears, assert that the workspace is read only then", func(t *testing.T) {
		source := &sessionSource{request: &Request{Repo: "https://github.com/o/r", ServiceCommand: []string{"/bin/sleep", "60"}}}
		b, dir := checkedOut(t, source)

		b.ServeService(serving(t), ServiceConfig{Dir: dir, Environment: os.Environ(), Executable: os.Args[0]})
		time.Sleep(1500 * time.Millisecond)

		if reads := source.reads(); reads != 0 {
			t.Fatalf("workspace reads before the Session identity = %d, want 0", reads)
		}
		if _, err := os.Stat(filepath.Join(dir, service.StatusFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status before the Session identity: %v, want none", err)
		}
		source.serve()
		waitForService(t, dir, "running")
	})

	t.Run("Given a Session that declares no service, when the runtime serves it, assert that a status left by an earlier start is removed", func(t *testing.T) {
		source := &sessionSource{request: &Request{Repo: "https://github.com/o/r"}}
		source.serve()
		b, dir := checkedOut(t, source)
		if err := service.WriteStatus(dir, service.Status{Command: []string{"old"}, State: "running"}); err != nil {
			t.Fatal(err)
		}

		b.ServeService(serving(t), ServiceConfig{Dir: dir, Environment: os.Environ(), Executable: os.Args[0]})

		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, service.StatusFile)); errors.Is(err, os.ErrNotExist) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the stale status was never removed")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("Given a source without a Session identity, when the service is served, assert that the workspace is never read", func(t *testing.T) {
		source := &fakeSource{request: &Request{Repo: "https://github.com/o/r", ServiceCommand: []string{"/bin/sleep", "60"}}}
		b := newBootstrapper(t, source, &fakeRunner{})

		b.ServeService(serving(t), ServiceConfig{Dir: t.TempDir(), Environment: os.Environ(), Executable: os.Args[0]})
		time.Sleep(200 * time.Millisecond)

		if source.calls != 0 {
			t.Fatalf("workspace reads = %d, want 0", source.calls)
		}
	})
}

// sessionSource is a control-plane source: it names a Session once serve is
// called.
type sessionSource struct {
	mu      sync.Mutex
	request *Request
	served  bool
	calls   int
}

func (s *sessionSource) serve() { s.mu.Lock(); s.served = true; s.mu.Unlock() }

func (s *sessionSource) reads() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func (s *sessionSource) SessionID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.served {
		return "", errors.New("read runtime identity: no such file")
	}
	return "00000000-0000-4000-8000-000000000001", nil
}

func (s *sessionSource) Workspace(context.Context) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.request, nil
}

// checkedOut returns a Bootstrapper whose checkout is already done, and the
// service directory.
func checkedOut(t *testing.T, source Source) (*Bootstrapper, string) {
	t.Helper()
	b := newBootstrapper(t, source, &fakeRunner{})
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (markers{b.stateDir}).markDone(context.Background(), checkout{Repo: "https://github.com/o/r"}); err != nil {
		t.Fatal(err)
	}
	return b, filepath.Join(t.TempDir(), "workspace-service")
}

// serving is a context that stops the service when the test ends and waits for
// its processes to go.
func serving(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		time.Sleep(200 * time.Millisecond)
	})
	return ctx
}

func waitForService(t *testing.T, dir, state string) service.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status service.Status
		if data, err := os.ReadFile(filepath.Join(dir, service.StatusFile)); err == nil && json.Unmarshal(data, &status) == nil && status.State == state {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the service never reached %s", state)
	return service.Status{}
}

func waitForLog(t *testing.T, dir, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dir, service.LogFile)); err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(filepath.Join(dir, service.LogFile))
	t.Fatalf("log = %q, want %q", data, want)
}
