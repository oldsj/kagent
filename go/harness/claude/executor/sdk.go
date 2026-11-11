package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/driver"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// DriverEnvName selects the Claude driver from the Harness environment:
// "process" (default) runs one `claude -p` per turn; "sdk" runs the Agent SDK
// runner as one live conversation shared by A2A turns and runner clients.
const DriverEnvName = "KAGENT_CLAUDE_DRIVER"

// ClientAddr is the SDK runner's client port, reached through the Substrate
// router CONNECT.
const ClientAddr = ":7682"

// imageSDKRunner is the harness image's SDK bridge: Node from Alpine on PATH
// and the bridge with its pinned SDK under /opt/kagent/sdk-runner. A composed
// runtime's configuration names the payload's instead.
var imageSDKRunner = config.SDKRunner{Node: "node", Script: "/opt/kagent/sdk-runner/runner.mjs"}

type driverMode string

const (
	modeProcess driverMode = "process"
	modeSDK     driverMode = "sdk"
)

func selectedDriver(environment []string) (driverMode, error) {
	value := ""
	for _, item := range environment {
		if name, v, ok := strings.Cut(item, "="); ok && name == DriverEnvName {
			value = v
		}
	}
	switch value {
	case "", string(modeProcess):
		return modeProcess, nil
	case string(modeSDK):
		return modeSDK, nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", DriverEnvName, modeProcess, modeSDK, value)
	}
}

// currentDriver holds the configured process driver whose configuration the
// SDK bridge compiles; workspace setup may replace it.
type currentDriver struct {
	mu     sync.Mutex
	driver *driver.ProcessDriver
}

func (c *currentDriver) get() *driver.ProcessDriver {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.driver
}

func (c *currentDriver) set(d *driver.ProcessDriver) {
	c.mu.Lock()
	c.driver = d
	c.mu.Unlock()
}

type sdkWiring struct {
	session *sdkrunner.Session
	current *currentDriver
	// prepare is set once Git preparation is configured.
	prepare func(context.Context) error
}

func newSDKWiring(ctx context.Context, cfg Config, bridge config.SDKRunner, base *driver.ProcessDriver, store *continuation.Store, grace time.Duration) (*sdkWiring, error) {
	if _, err := exec.LookPath(bridge.Node); err != nil {
		return nil, fmt.Errorf("the SDK runner needs Node in the runtime: %w", err)
	}
	if _, err := os.Stat(bridge.Script); err != nil {
		return nil, fmt.Errorf("the SDK runner bridge is missing from the runtime: %w", err)
	}
	w := &sdkWiring{current: &currentDriver{driver: base}}
	dir := filepath.Join(cfg.DataDir, "sdk-runner")
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: sdkrunner.NodeLauncher{Node: bridge.Node, Script: bridge.Script, ICUDataDir: bridge.ICUDataDir, Dir: "/", Env: bridgeEnvironment(cfg.Environment)},
		Spec: func(profile sdkrunner.Profile) (sdkrunner.Spec, error) {
			return w.current.get().SDKSpec(profile, filepath.Join(dir, "home"))
		},
		Prepare: func(ctx context.Context) error {
			if w.prepare == nil {
				return nil
			}
			return w.prepare(ctx)
		},
		Continuation: store, Activity: cfg.Activity, Dir: dir, StopGrace: grace,
		Logger: logging.FromContext(ctx),
		ClientGate: func() error {
			waiting, err := w.current.get().AwaitingReplay()
			if err != nil {
				return fmt.Errorf("read the pending approval: %w", err)
			}
			if waiting {
				return errors.New("the conversation is waiting for an answer to a pending approval or question")
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	w.session = session
	return w, nil
}

// driver returns the turn runner for a configured process driver.
func (w *sdkWiring) driver(d *driver.ProcessDriver) *driver.SDKDriver {
	w.current.set(d)
	return driver.NewSDKDriver(d, w.session)
}

// serve starts the client port and the orphan reaper. The returned closer
// stops both and the bridge.
func (w *sdkWiring) serve(ctx context.Context, addr string) (io.Closer, error) {
	ctx, cancel := context.WithCancel(ctx)
	go w.session.ReapOrphans(ctx, 5*time.Second)
	if addr == "" {
		return closerFunc(func() error { cancel(); return w.session.Close() }), nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("listen on SDK runner client port: %w", err)
	}
	server := &http.Server{Handler: sdkrunner.NewServer(ctx, w.session), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.FromContext(ctx).ErrorContext(ctx, "sdk runner client port stopped", "error", err)
		}
	}()
	return closerFunc(func() error {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		return errors.Join(server.Shutdown(shutdown), w.session.Close())
	}), nil
}

// bridgeEnvironment is the Node process's own environment. The Claude CLI's
// environment travels separately in the start spec. The controller payloads
// are large and of no use to the bridge.
func bridgeEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, "KAGENT_CONFIG_JSON=") || strings.HasPrefix(item, "KAGENT_AGENT_CARD_JSON=") {
			continue
		}
		result = append(result, item)
	}
	return result
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

type closers []io.Closer

func (c closers) Close() error {
	var errs []error
	for _, closer := range c {
		errs = append(errs, closer.Close())
	}
	return errors.Join(errs...)
}
