package workspace

import (
	"context"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime/service"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

// ServiceDir is where the declared workspace service's status and logs live,
// on the durable disk and outside the checkout.
const ServiceDir = "/data/workspace-service"

// Bounds of the retries before the service starts: reading the Session's
// workspace and waiting for its checkout and preparation.
const (
	serviceRetryMin = time.Second
	serviceRetryMax = 30 * time.Second
)

// ServiceConfig configures ServeService.
type ServiceConfig struct {
	// Dir holds the status file and logs, normally ServiceDir.
	Dir string
	// Environment is the environment the agent's tools get.
	Environment []string
	// Executable is the harness binary for the supervisor; empty uses this
	// process's executable.
	Executable string
}

// ServeService starts the Session's declared workspace service in the
// background, once per runtime process: at a cold start and at every resume,
// since a resume restarts the process. Once the process serves a Session, it
// reads the Session's workspace, waits until the checkout and any preparation
// are ready, as a turn would, and then runs the command in the checkout until
// ctx ends. It does not wait for a
// turn, so a Session woken only by preview traffic still starts its service.
// A Session that declares no service gets no status file. The service is not
// restarted when it exits.
func (b *Bootstrapper) ServeService(ctx context.Context, cfg ServiceConfig) {
	go b.serveService(ctx, cfg)
}

func (b *Bootstrapper) serveService(ctx context.Context, cfg ServiceConfig) {
	// Only a control-plane source names a Session; a configuration check or an
	// embedder without one has no service to run.
	identity, ok := b.source.(sessionIdentity)
	if !ok {
		return
	}
	logger := logging.FromContext(ctx)
	// The process may resume from a golden memory snapshot taken before it
	// served a Session, so wait for the Session identity rather than reading
	// the workspace once at startup.
	if !waitForSession(ctx, identity) {
		return
	}
	var request *Request
	if !retryService(ctx, func() bool {
		var err error
		request, err = b.source.Workspace(ctx)
		if err != nil {
			logger.WarnContext(ctx, "workspace service: read the Session's workspace", "error", err)
		}
		return err == nil
	}) {
		return
	}
	if request == nil || len(request.ServiceCommand) == 0 {
		if err := service.RemoveStatus(cfg.Dir); err != nil {
			logger.WarnContext(ctx, "workspace service: remove a stale status", "error", err)
		}
		return
	}
	command := request.ServiceCommand
	if err := service.WriteStatus(cfg.Dir, service.Status{Command: command, State: "starting"}); err != nil {
		logger.WarnContext(ctx, "workspace service: record status", "error", err)
	}
	if !retryService(ctx, func() bool {
		failure := b.Ready(ctx)
		if failure != "" {
			logger.InfoContext(ctx, "workspace service: waiting for the workspace", "reason", failure)
		}
		return failure == ""
	}) {
		return
	}
	err := service.Run(ctx, service.Spec{Command: command, WorkDir: b.dir, Dir: cfg.Dir, Environment: cfg.Environment, Executable: cfg.Executable}, logger)
	if err != nil && ctx.Err() == nil {
		logger.WarnContext(ctx, "workspace service stopped", "error", err)
	}
}

type sessionIdentity interface {
	SessionID() (string, error)
}

func waitForSession(ctx context.Context, identity sessionIdentity) bool {
	ticker := time.NewTicker(serviceRetryMin)
	defer ticker.Stop()
	for {
		if _, err := identity.SessionID(); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// retryService calls attempt with capped exponential backoff until it
// succeeds, and reports false if ctx ended first.
func retryService(ctx context.Context, attempt func() bool) bool {
	delay := serviceRetryMin
	for {
		if attempt() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		delay = min(2*delay, serviceRetryMax)
	}
}
