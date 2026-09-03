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

	"github.com/bejusxd/rotup/internal/adapters/sqlite"
	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/domain"
	"github.com/bejusxd/rotup/internal/web"
)

const shutdownTimeout = 5 * time.Second

// Application owns the M1 daemon lifecycle and resources.
type Application struct {
	cfg       config.Config
	logger    *slog.Logger
	store     *sqlite.Store
	server    *http.Server
	status    atomic.Value
	closeOnce sync.Once
	closeErr  error
}

// New initializes persistence, applies migrations, and records the M1 demo run.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*Application, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("application starting", "version", version)

	store, err := sqlite.Open(ctx, cfg.DatabasePath, logger)
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}

	application := &Application{
		cfg:    cfg,
		logger: logger,
		store:  store,
	}
	application.status.Store("starting")

	if err := application.recordDemoRun(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	handler, err := web.NewHandler(application, version)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("initialize HTTP handlers: %w", err)
	}
	application.server = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return application, nil
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
		Status:      domain.RunStatusSucceeded,
		Summary:     "M1 demo run: application -> SQLite -> dashboard; this is not a backup",
		RequestedAt: now,
		StartedAt:   now,
		FinishedAt:  now,
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

// DatabaseHealth reports whether the M1 SQLite store can answer queries.
func (a *Application) DatabaseHealth(ctx context.Context) error {
	return a.store.Health(ctx)
}

// LatestRun supplies the latest M1 history record to the dashboard.
func (a *Application) LatestRun(ctx context.Context) (*domain.Run, error) {
	return a.store.LatestRun(ctx)
}

// Close releases application resources. It is safe to call more than once.
func (a *Application) Close() error {
	a.closeOnce.Do(func() {
		a.status.Store("stopped")
		a.closeErr = a.store.Close()
		if a.closeErr != nil {
			a.logger.Error("SQLite close failed", "error", a.closeErr)
		} else {
			a.logger.Info("application stopped")
		}
	})
	return a.closeErr
}
