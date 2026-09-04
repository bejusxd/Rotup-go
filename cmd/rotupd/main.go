package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bejusxd/rotup/internal/app"
	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/observability"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	// A subcommand is provisioning or diagnostics and must work on a host whose
	// configuration is not valid yet -- that is often exactly why doctor is
	// being run -- so it is dispatched before the daemon loads its configuration.
	if code, handled := dispatch(os.Args[1:], os.Stdout, os.Stderr); handled {
		return code
	}

	cfg, err := config.Load()
	if err != nil {
		fprintErr("invalid configuration: %v\n", err)
		fprintErr("Run `rotupd doctor` to see what this host is missing.\n")
		return 1
	}

	logger := observability.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, logger, version)
	if err != nil {
		logger.Error("application initialization failed", "error", err)
		return 1
	}

	runErr := application.Run(ctx)
	closeErr := application.Close()
	if runErr != nil {
		logger.Error("application stopped with an error", "error", runErr)
		return 1
	}
	if closeErr != nil {
		logger.Error("resource cleanup failed", "error", closeErr)
		return 1
	}

	return 0
}
