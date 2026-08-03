package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
)

type BuildInfo struct{ Version, Commit string }

func Run(ctx context.Context, cfg config.Config, build BuildInfo, logger *slog.Logger) error {
	shutdownTelemetry, err := telemetry.Configure(ctx, cfg.OTLPEndpoint, build.Version, cfg.Environment)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()

	pool, err := migrations.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	databaseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cfg.MigrateOnly {
		if err := migrations.Migrate(databaseCtx, pool); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
		if err := migrations.GrantRuntimePrivileges(databaseCtx, pool, cfg.RuntimeDBRole); err != nil {
			return fmt.Errorf("grant runtime database privileges: %w", err)
		}
		logger.Info("database migrations complete")
		return nil
	}
	if err := migrations.ValidateCurrentSchema(databaseCtx, pool); err != nil {
		return fmt.Errorf("validate database schema: %w", err)
	}
	expectedSchemaVersion, err := migrations.LatestSchemaVersion()
	if err != nil {
		return err
	}

	logger.Info("control plane starting", "role", cfg.Role)
	componentCtx, stopComponents := context.WithCancel(ctx)
	defer stopComponents()
	var sliceService *localslice.Service
	workerErr := make(chan error, 1)
	var trust *trustserver.Server
	trustErr := make(chan error, 1)
	if cfg.RunsWorker() && cfg.ControllerEndpointID != "" {
		trust, err = trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.New(pool, cfg.ControllerEndpointID, build.Version)))
		if err != nil {
			return fmt.Errorf("configure trust server: %w", err)
		}
		go func() { trustErr <- trust.Serve() }()
	}
	stopTrust := func() error {
		if trust == nil {
			return nil
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		return trust.Shutdown(shutdownCtx)
	}
	if cfg.LocalSimulator {
		sliceService = localslice.New(pool)
		transport, err := transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue)
		if err != nil {
			return fmt.Errorf("configure transport client: %w", err)
		}
		if cfg.RunsWorker() {
			worker := localslice.NewWorker(sliceService, transport, logger)
			go func() { workerErr <- worker.Run(componentCtx) }()
		}
	}
	if !cfg.RunsAPI() {
		select {
		case <-ctx.Done():
			if err := stopTrust(); err != nil {
				return fmt.Errorf("shutdown trust UDS: %w", err)
			}
			return ctx.Err()
		case err := <-workerErr:
			_ = stopTrust()
			return fmt.Errorf("run local slice worker: %w", err)
		case err := <-trustErr:
			_ = stopTrust()
			return fmt.Errorf("serve trust UDS: %w", err)
		}
	}

	server := api.New(cfg.HTTPAddress, pool, api.BuildInfo{Version: build.Version, Commit: build.Commit, Role: string(cfg.Role)}, logger, cfg.BodyLimit, cfg.RequestTimeout, operationAuthEnabled(cfg), cfg.DevAuthToken, expectedSchemaVersion)
	if cfg.ControllerEndpointID != "" {
		transport, transportErr := transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue)
		if transportErr != nil {
			return fmt.Errorf("configure enrollment transport: %w", transportErr)
		}
		server.EnableEnrollment(enrollment.New(pool, cfg.ControllerEndpointID, build.Version), transport)
	}
	if sliceService != nil {
		server.EnableLocalSlice(sliceService)
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()

	select {
	case err := <-serverErr:
		_ = stopTrust()
		return fmt.Errorf("serve HTTP: %w", err)
	case err := <-workerErr:
		stopComponents()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		_ = stopTrust()
		return fmt.Errorf("run local slice worker: %w", err)
	case err := <-trustErr:
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		_ = stopTrust()
		return fmt.Errorf("serve trust UDS: %w", err)
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		if err := stopTrust(); err != nil {
			return fmt.Errorf("shutdown trust UDS: %w", err)
		}
		err := <-serverErr
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		logger.Info("control plane stopped", "role", cfg.Role)
		return ctx.Err()
	}
}

func operationAuthEnabled(cfg config.Config) bool {
	return cfg.DevAuth
}
