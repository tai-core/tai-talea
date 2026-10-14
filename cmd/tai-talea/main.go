// Command tai-talea is the capacity control plane for SGLang containerised
// instances, described in SGLang容器化实例资源管控工具开发文档.md.
//
// It receives partner capacity events, prepares containers, starts and registers
// SGLang Prefill/Decode services with sglang-router, drains through Router
// readiness and returns containers to the partner.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tai-core/tai-talea/internal/api"
	"github.com/tai-core/tai-talea/internal/buildinfo"
	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/controller"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/instancemanager"
	"github.com/tai-core/tai-talea/internal/launcher"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/onboarding"
	"github.com/tai-core/tai-talea/internal/partner"
	"github.com/tai-core/tai-talea/internal/partners"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/routeradapter"
	"github.com/tai-core/tai-talea/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	flags := flag.NewFlagSet("tai-talea", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "path to the yaml configuration file (required)")
	checkOnly := flags.Bool("check-config", false, "validate the configuration and exit")
	printVersion := flags.Bool("version", false, "print build information and exit")
	logLevel := flags.String("log-level", envOr("TAI_TALEA_LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *printVersion {
		fmt.Println("tai-talea " + buildinfo.Summary())
		return 0
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "tai-talea: --config is required")
		flags.Usage()
		return 2
	}

	logger := newLogger(*logLevel)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load configuration", "error", err.Error())
		return 1
	}
	if *checkOnly {
		fmt.Printf("configuration %s is valid (%d partners, router %s)\n", *configPath, len(cfg.Partners), cfg.Router.URL)
		return 0
	}
	if err := serve(cfg, logger); err != nil {
		logger.Error("control plane stopped", "error", err.Error())
		return 1
	}
	return 0
}

func serve(cfg config.Config, logger *slog.Logger) error {
	metrics := obs.NewRegistry()

	sinks := []obs.AlertSink{obs.LogSink{Logger: logger}}
	if cfg.Alerts.WebhookURL != "" {
		sinks = append(sinks, obs.WebhookSink{URL: cfg.Alerts.WebhookURL})
	}
	alerter := obs.NewAlerter(metrics, logger, cfg.Alerts.Cooldown.Duration(), sinks...)

	persistence, err := store.OpenSQLite(cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer persistence.Close()

	routerClient, err := routeradapter.New(cfg.Router.ToAdapter())
	if err != nil {
		return fmt.Errorf("router adapter: %w", err)
	}
	launcherClient, err := launcher.New(
		cfg.Controller.CallTimeout.Duration(), cfg.Controller.BootstrapToken)
	if err != nil {
		return fmt.Errorf("bootstrap client: %w", err)
	}
	manager, err := instancemanager.New(launcherClient, instancemanager.Profile{
		OS:               cfg.ImageProfile.OS,
		CUDA:             cfg.ImageProfile.CUDA,
		Python:           cfg.ImageProfile.Python,
		SGLang:           cfg.ImageProfile.SGLang,
		Wheelhouse:       cfg.ImageProfile.Wheelhouse,
		Virtualenv:       cfg.ImageProfile.Virtualenv,
		BootstrapVersion: bootstrapVersion,
		ModelID:          cfg.Controller.ModelID,
		MaxLeaseAge:      cfg.Controller.LeaseWarning.Duration(),
	})
	if err != nil {
		return fmt.Errorf("instance manager: %w", err)
	}
	// CCI maps only the container's HTTP ports onto a public ingress, and the PD
	// transfer handshake listens on a port it never publishes at all, so peers
	// have to be told the container's own address. That address exists nowhere
	// in the platform API: the install pipeline measures it from inside and the
	// enrolled row is where it is kept. An unknown or released instance has
	// none, and then the adapter keeps the published endpoint.
	resolveAdvertiseHost := func(name string) string {
		row, err := persistence.GetInstance(context.Background(), name)
		if err != nil || !row.Active() {
			return ""
		}
		parsed, err := url.Parse(domain.NormalizeEndpoint(row.Endpoint))
		if err != nil {
			return ""
		}
		return parsed.Hostname()
	}
	registry, err := partners.Build(cfg, resolveAdvertiseHost)
	if err != nil {
		return err
	}
	plannerInstance, err := planner.New(cfg.Planner.ToPlanner())
	if err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	// The planner can ask for capacity, but the creation pipeline needs the SSH
	// onboarding intake, which is built after the controller. The bridge is
	// bound to the API server as soon as it exists.
	capacity := &capacityBridge{}

	ctrl, err := controller.New(controller.Deps{
		Config:   cfg,
		Store:    persistence,
		Router:   routerClient,
		Launcher: launcherClient,
		Manager:  manager,
		Partners: registry,
		Planner:  plannerInstance,
		Capacity: capacity,
		Metrics:  metrics,
		Alerter:  alerter,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	var ready atomic.Bool
	var onboards *onboarding.Manager
	if cfg.Onboarding.Script != "" {
		if cfg.Onboarding.Python == "" {
			cfg.Onboarding.Python = "python3"
		}
		onboards, err = onboarding.New(cfg.Onboarding.StateDir, persistence, onboarding.ProcessRunner(cfg.Onboarding, cfg.Controller.BootstrapToken), ctrl.EnrollPrepared)
		if err != nil {
			return fmt.Errorf("onboarding: %w", err)
		}
	}
	apiServer, err := api.New(api.Options{
		Onboarding: onboards,
		Config:     cfg,
		Store:      persistence,
		Controller: ctrl,
		Metrics:    metrics,
		Logger:     logger,
		Partners:   registry,
		Ready:      ready.Load,
	})
	if err != nil {
		return err
	}
	capacity.bind(apiServer)
	logger.Info("planner configuration",
		"mode", plannerInstance.Mode(), "gear", cfg.Planner.Gear,
		"capacity_actions", cfg.Planner.CapacityActions,
		"provision_timeout", cfg.Controller.ProvisionTimeout.Duration().String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Recovery runs before the HTTP listener so no capacity event can race the
	// restart reconciliation.
	recoveryCtx, cancelRecovery := context.WithTimeout(ctx, 90*time.Second)
	report, resumed, err := ctrl.Recover(recoveryCtx)
	cancelRecovery()
	if err != nil {
		logger.Error("startup recovery failed", "error", err.Error())
	} else {
		logger.Info("startup recovery complete",
			"checked", report.Checked, "lost", len(report.Lost),
			"recovered", len(report.Recovered), "reregistered", len(report.Reregister),
			"resumed_operations", len(resumed), "resumed_events", report.ResumedEvents)
	}
	if err := ctrl.PublishGauges(ctx); err != nil {
		logger.Warn("initial gauge publication failed", "error", err.Error())
	}
	ready.Store(true)

	go ctrl.RunPeriodic(ctx)
	if onboards != nil {
		go onboards.Run(ctx)
	}
	if cfg.Pull.Enabled {
		puller := partner.NewPuller(persistence, metrics, alerter, logger, cfg.Pull.FailureThreshold)
		for _, adapter := range registry.All() {
			schedule := controller.PullSchedule{Adapter: adapter, Puller: puller, Interval: cfg.Pull.Interval.Duration()}
			logger.Info("partner pull enabled", "partner_id", adapter.PartnerID(), "interval", cfg.Pull.Interval.Duration().String())
			go ctrl.RunPull(ctx, schedule)
		}
	}

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration(),
		WriteTimeout:      cfg.Server.WriteTimeout.Duration(),
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("capacity control plane listening",
			"address", cfg.Server.Listen,
			"router", cfg.Router.URL,
			"gear", cfg.Planner.Gear,
			"partners", strings.Join(registry.IDs(), ","))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received; draining http server")
	shutdownTimeout := cfg.Server.ShutdownTimeout.Duration()
	if shutdownTimeout <= 0 {
		shutdownTimeout = 10 * time.Second
	}
	return api.Shutdown(context.Background(), server, shutdownTimeout)
}

// bootstrapVersion is the runtime launcher contract the control plane requires
// from a container (development document §9 and §10).
//
// It is bumped whenever the container side of that contract changes, because
// the control plane cannot inspect a worker image - it only knows what that
// image's bootstrap claims about itself. /2 is the version whose HTTP surface
// includes GET /bootstrap/liveness, the endpoint the worker livenessProbe
// points at. A worker still answering /1 is refused during preparation with
// bootstrap_version_mismatch, and that refusal is the entire point: the other
// pairing - probe enabled, image without the endpoint - fails as a 404 that the
// kubelet reads as a dead container, so the worker restarts forever without
// ever serving.
//
// Moving this value is a two-sided change. The worker image must carry a
// matching bootstrap, and this binary must be rebuilt and rolled; either half
// alone leaves the two disagreeing. That disagreement is loud on purpose.
const bootstrapVersion = "tai-talea-bootstrap/2"

// capacityBridge hands the controller a way to ask for capacity without giving
// it the provisioning pipeline. The pipeline needs the SSH onboarding intake,
// which is created after the controller, so the bridge is bound once the API
// server exists and stays empty until then.
//
// The count comes from the API server's own job tracker: a creation the platform
// has queued is invisible everywhere else, so that tracker is the only place
// that knows capacity is already on its way.
type capacityBridge struct {
	mu     sync.RWMutex
	server *api.Server
}

func (b *capacityBridge) bind(server *api.Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.server = server
}

func (b *capacityBridge) RequestCapacity(ctx context.Context, partnerID string) (string, error) {
	b.mu.RLock()
	server := b.server
	b.mu.RUnlock()
	if server == nil {
		return "", errors.New("provisioning pipeline is not available yet")
	}
	record, err := server.RequestCapacity(ctx, partnerID, "")
	if err != nil {
		return "", err
	}
	return record.Name, nil
}

func (b *capacityBridge) OutstandingCapacity() int { return api.OutstandingProvisions() }

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed})
	return slog.New(handler)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
