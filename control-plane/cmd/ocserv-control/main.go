package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GentleKingson/ocservia/control-plane/internal/platform/app"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cfg, err := config.Load(os.Args[1:], os.LookupEnv)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()})).With(
		"service.name", "ocserv-control",
		"service.version", version,
		"environment", cfg.Environment,
	)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, app.BuildInfo{Version: version, Commit: commit}, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}
