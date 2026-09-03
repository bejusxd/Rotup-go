package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bejusxd/rotup/internal/adapters/rclone"
	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/collector"
	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/platform/process"
	"github.com/bejusxd/rotup/internal/secrets"
	"github.com/bejusxd/rotup/internal/staging"
	"github.com/bejusxd/rotup/internal/web"
)

const shutdownTimeout = 5 * time.Second

// Application owns the daemon lifecycle and resources.
type Application struct {
	cfg     config.Config
	logger  *slog.Logger
	store   *sqlite.Store
	layout  *staging.Layout
	secrets *secrets.FileStore
	engine  *rclone.Engine
	work    *collector.Service
	admin   *collector.Admin
	server  *http.Server

	// workCancel stops background collection work at shutdown.
	workCancel context.CancelFunc

	status      atomic.Value
	engineState atomic.Value
	closeOnce   sync.Once
	closeErr    error
}

// New initializes persistence, staging, secrets, the transfer engine, and the
// HTTP surface, then runs startup recovery before any new work is accepted.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*Application, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("application starting", "version", version)

	store, err := sqlite.Open(ctx, cfg.DatabasePath, logger)
	if err != nil {
		return nil, err
	}
	application := &Application{cfg: cfg, logger: logger, store: store}
	application.status.Store("starting")
	application.engineState.Store(web.EngineStatus{})

	if err := application.initialize(ctx, version); err != nil {
		_ = store.Close()
		return nil, err
	}
	return application, nil
}

func (a *Application) initialize(ctx context.Context, version string) error {
	if _, err := a.store.Migrate(ctx); err != nil {
		return err
	}
	if err := a.store.IntegrityCheck(ctx); err != nil {
		return err
	}

	layout, err := staging.NewLayout(a.cfg.StagingRoot)
	if err != nil {
		return err
	}
	a.layout = layout
	a.logger.Info("staging root ready", "path", layout.Root())

	secretStore, err := secrets.NewFileStore(a.cfg.SecretRoot)
	if err != nil {
		return err
	}
	a.secrets = secretStore

	a.engine = rclone.New(rclone.Config{
		ExecutablePath: a.cfg.RclonePath,
		CollectTimeout: a.cfg.CollectionTimeout,
	}, process.NewRunner(), secretStore, secretStore, a.logger)
	a.probeEngine(ctx)

	workCtx, cancel := context.WithCancel(context.Background())
	a.workCancel = cancel
	a.work = collector.New(workCtx, collector.Options{
		Store:  a.store,
		Engine: a.engine,
		Layout: layout,
		Logger: a.logger,
	})
	a.admin = collector.NewAdmin(a.store, secretStore, layout, a.logger)

	// Recovery runs before the HTTP surface exists, so no new work can race it.
	report, err := a.work.Recover(ctx)
	if err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}
	if report.InterruptedRuns > 0 || report.RecoveredSources > 0 {
		a.logger.Warn("startup recovery completed",
			"interrupted_runs", report.InterruptedRuns,
			"sources_marked_for_recovery", report.RecoveredSources)
	}

	if err := a.recordDemoRun(ctx); err != nil {
		return err
	}

	handler, err := web.NewHandler(web.Deps{
		Version: version,
		Status:  a,
		Repo:    a.store,
		Admin:   a.admin,
		Runner:  a.work,
		Staging: layout,
		Engine:  a.EngineStatus,
		Logger:  a.logger,
	})
	if err != nil {
		return fmt.Errorf("initialize HTTP handlers: %w", err)
	}
	a.server = &http.Server{
		Addr:              a.cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return nil
}

// probeEngine records the transfer engine's availability. A missing engine is a
// degraded configuration, not a fatal error: the rest of the daemon still runs
// and the UI explains why collection is unavailable.
func (a *Application) probeEngine(ctx context.Context) {
	status := web.EngineStatus{Name: "rclone", DiscoveredOnPath: a.cfg.RcloneFromPath}
	if !a.engine.Available() {
		status.Detail = "no rclone executable is configured; set ROTUP_RCLONE_PATH"
		a.engineState.Store(status)
		a.logger.Warn("transfer engine unavailable", "reason", status.Detail)
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := a.engine.Probe(probeCtx)
	if err != nil {
		status.Detail = "the configured rclone executable could not be probed"
		a.engineState.Store(status)
		a.logger.Warn("transfer engine probe failed", "path", a.cfg.RclonePath, "error", err)
		return
	}
	status.Available = true
	status.Version = info.Version
	a.engineState.Store(status)
	a.logger.Info("transfer engine ready", "engine", info.Name, "version", info.Version, "path", info.Path)
	if a.cfg.RcloneFromPath {
		a.logger.Warn("rclone was discovered on the inherited PATH; set ROTUP_RCLONE_PATH explicitly for production")
	}
}

// EngineStatus reports the discovered transfer engine to the UI.
func (a *Application) EngineStatus() web.EngineStatus {
	return a.engineState.Load().(web.EngineStatus)
}

func (a *Application) recordDemoRun(ctx context.Context) error {
	id, err := domain.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID:          id,
		Kind:        domain.RunKindDemo,
		TriggerKind: domain.RunTriggerManual,
		Status:      domain.RunStatusSucceeded,
		Summary:     "M1 demo run: application -> SQLite -> dashboard; this is not a backup",
		RequestedAt: now,
		StartedAt:   now,
		FinishedAt:  now,
		UpdatedAt:   now,
	}
	if err := a.store.RecordRun(ctx, run); err != nil {
		return fmt.Errorf("record M1 demo run: %w", err)
	}
	a.logger.Info("M1 fake run recorded", "run_id", run.ID, "kind", run.Kind, "status", run.Status)
	return nil
}

// Run listens for HTTP requests until the context is canceled or the server fails.
func (a *Application) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.HTTPAddr, err)
	}
	return a.serve(ctx, listener)
}

func (a *Application) serve(ctx context.Context, listener net.Listener) error {
	a.status.Store("running")
	a.logger.Info("HTTP server started", "address", listener.Addr().String())

	serverResult := make(chan error, 1)
	go func() {
		serverResult <- a.server.Serve(listener)
	}()

	select {
	case err := <-serverResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		a.status.Store("degraded")
		a.logger.Error("HTTP server failed", "error", err)
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		a.status.Store("stopping")
		a.logger.Info("shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			_ = a.server.Close()
			<-serverResult
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		if err := <-serverResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	}
}

// ApplicationStatus returns the daemon lifecycle status for the dashboard.
func (a *Application) ApplicationStatus() string {
	return a.status.Load().(string)
}

// DatabaseHealth reports whether the SQLite store can answer queries.
func (a *Application) DatabaseHealth(ctx context.Context) error {
	return a.store.Health(ctx)
}

// LatestRun supplies the latest history record to the dashboard.
func (a *Application) LatestRun(ctx context.Context) (*domain.Run, error) {
	return a.store.LatestRun(ctx)
}

// Close releases application resources. It is safe to call more than once.
func (a *Application) Close() error {
	a.closeOnce.Do(func() {
		a.status.Store("stopped")
		if a.workCancel != nil {
			// Cancelling first lets an in-flight external tool be stopped, then
			// the wait makes sure its run row reaches a terminal state before
			// the database closes underneath it.
			a.workCancel()
		}
		if a.work != nil {
			a.work.Wait()
		}
		a.closeErr = a.store.Close()
		if a.closeErr != nil {
			a.logger.Error("SQLite close failed", "error", a.closeErr)
		} else {
			a.logger.Info("application stopped")
		}
	})
	return a.closeErr
}
