// Command kagent-claude runs the Claude Harness runtime adapter.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/kagent-dev/kagent/go/adk/pkg/activity"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/adk/pkg/bootstrap"
	"github.com/kagent-dev/kagent/go/adk/pkg/controllerclient"
	"github.com/kagent-dev/kagent/go/adk/pkg/taskstore"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/executor"
	"github.com/kagent-dev/kagent/go/harness/runtime/service"
	"github.com/kagent-dev/kagent/go/harness/runtime/workspace"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/telemetry"
)

const (
	dataDir     = "/data"
	privatePort = "80"
)

func main() {
	// The workspace service supervisor is this binary re-executed; it runs
	// nothing else of the harness.
	if len(os.Args) > 1 && os.Args[1] == service.SupervisorArg {
		os.Exit(service.Supervise(os.Args[2:]))
	}
	check := flag.Bool("check", false, "validate configuration and Claude version, then exit")
	flag.Parse()
	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := logging.IntoContext(context.Background(), logger)
	if err := run(ctx, *check, os.Getenv, os.Environ()); err != nil { //nolint:forbidigo // Reads only controller-generated config and agent-card payloads.
		logger.ErrorContext(ctx, "claude harness stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, check bool, getenv func(string) string, environment []string) error {
	bundle, err := bootstrap.FromEnvironment(ctx, getenv)
	if err != nil {
		return err
	}
	configJSON, agentCardJSON := bundle.ConfigJson, bundle.AgentCardJson
	var card a2atype.AgentCard
	if err := json.Unmarshal(agentCardJSON, &card); err != nil {
		return fmt.Errorf("decode agent card: %w", err)
	}
	if strings.TrimSpace(card.Name) == "" {
		return fmt.Errorf("agent card name is required")
	}
	// Telemetry identity and capture policy are compiled into the runtime
	// configuration, so the configuration is read before tracing starts.
	cfg, err := config.Parse(configJSON)
	if err != nil {
		return err
	}
	cfg.RuntimeTelemetry = cfg.RuntimeTelemetry.WithEnvironment(environment)
	logger, err := logging.New(os.Stderr, cmp.Or(cfg.LogLevel, "info"))
	if err != nil {
		return err
	}
	ctx = logging.IntoContext(ctx, logger)
	providers, err := telemetry.Init(ctx, telemetry.Options{
		Config: cfg.RuntimeTelemetry.ExporterConfig(), Identity: cfg.RuntimeTelemetry.ResourceIdentity(), Defaults: cfg.RuntimeTelemetry.ResourceDefaults(card.Name),
		// Substrate restores every session from one snapshot taken after Init, so metrics started
		// here would give all sessions one identity and their totals would overwrite each other.
		DeferMetrics: true,
	})
	if err != nil {
		logging.FromContext(ctx).ErrorContext(ctx, "failed to initialize harness telemetry", "error", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := providers.Shutdown(shutdownCtx); err != nil {
			logging.FromContext(ctx).ErrorContext(ctx, "failed to shutdown harness telemetry", "error", err)
		}
	}()

	var controller *controllerclient.Client
	var source workspace.Source
	// A serving runtime always opens the control plane: a fork's fresh-conversation
	// flag comes through it whether or not the configuration enables Git.
	if check {
		if cfg.Git != nil {
			source = workspace.Unavailable()
		}
	} else {
		if controller, source, err = workspace.OpenController(cfg.ControlPlaneURL, card.Name); err != nil {
			return err
		}
		defer controller.Close()
	}
	execCfg := executor.Config{ConfigJSON: configJSON, DataDir: dataDir, Environment: environment, Workspace: source, Conversation: workspace.ConversationOf(source)}
	if !check {
		execCfg.ClientAddr = executor.ClientAddr
		if controller != nil {
			store := taskstore.New(controller, apia2a.RuntimeIdentityPath)
			reporter := activity.NewReporter(store, logging.FromContext(ctx))
			go reporter.Run(ctx)
			// A wake that starts no turn must still become eligible for suspension.
			go reporter.IdleWhenServing(ctx, store.ServesSession, time.Second)
			execCfg.Activity = reporter
		}
	}
	exec, closer, err := executor.New(ctx, execCfg)
	if err != nil {
		return err
	}
	defer closer.Close()
	if check {
		return nil
	}
	application, err := app.New(app.AppConfig{
		AgentCard: card, Port: privatePort, AppName: card.Name, ControllerClient: controller, ControlPlaneURL: cfg.ControlPlaneURL, MaxContentLength: cfg.MaxContentLength,
		Logger: logging.FromContext(ctx), Telemetry: cfg.RuntimeTelemetry, Flush: providers.ForceFlush,
		HandlerOpts: []a2asrv.RequestHandlerOption{a2asrv.WithExecutorContextInterceptor(startMetrics(providers.StartMetrics))},
	}, exec)
	if err != nil {
		return fmt.Errorf("construct private A2A app: %w", err)
	}
	return application.Run()
}

// startMetrics starts metrics on the first execution, which runs after the snapshot restore.
type startMetrics func()

func (start startMetrics) Intercept(ctx context.Context, _ *a2asrv.ExecutorContext) (context.Context, error) {
	start()
	return ctx, nil
}
