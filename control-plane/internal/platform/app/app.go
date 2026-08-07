package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
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
	sliceService := localslice.New(pool)
	operationService := operationstore.NewWithConcurrency(pool, cfg.UserOperationConcurrency)
	workerErr := make(chan error, 2)
	maintenanceErr := make(chan error, 1)
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
	if cfg.RunsWorker() && (cfg.LocalSimulator || cfg.ControllerEndpointID != "") {
		transport, err := transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue)
		if err != nil {
			return fmt.Errorf("configure transport client: %w", err)
		}
		worker := localslice.NewWorker(sliceService, transport, logger)
		go func() { workerErr <- worker.Run(componentCtx) }()
		operationWorker, workerConfigErr := operationstore.NewWorker(operationService, transport, logger)
		if workerConfigErr != nil {
			return fmt.Errorf("configure outbox worker: %w", workerConfigErr)
		}
		go func() { workerErr <- operationWorker.Run(componentCtx) }()
	}
	telemetryService := telemetrystore.New(pool)
	userStateService := userstate.New(pool)
	userOperationsService := useroperations.NewWithConcurrency(pool, userStateService, cfg.UserOperationConcurrency)
	auditManager := audit.NewManager(pool, cfg.AuditCheckpointKey)
	var apiTransport *transportclient.Client
	if cfg.ControllerEndpointID != "" {
		apiTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue)
		if err != nil {
			return fmt.Errorf("configure API transport: %w", err)
		}
	}
	var certificateService *certificates.Service
	if cfg.CertificateSignerURL != "" {
		signer, signerErr := certificates.NewHTTPSigner(cfg.CertificateSignerURL, cfg.CertificateSignerToken, cfg.CertificateSignerTimeout)
		if signerErr != nil {
			return fmt.Errorf("configure external certificate signer: %w", signerErr)
		}
		certificateService = certificates.NewWithDependencies(pool, operationService, signer, signer, apiTransport)
	} else {
		certificateService = certificates.New(pool, operationService)
	}
	if cfg.RunsScheduler() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				started := time.Now()
				runCtx, span := otel.Tracer("ocservia.useroperations").Start(componentCtx, "user_operations.scheduler.run")
				if err := userOperationsService.RunOnce(runCtx); err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, "scheduler run failed")
					span.End()
					logger.ErrorContext(componentCtx, "user operations scheduler failed", "alert_kind", "user_operations.scheduler_failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
					maintenanceErr <- err
					return
				}
				span.End()
				logger.InfoContext(componentCtx, "user operations scheduler completed", "duration_ms", time.Since(started).Milliseconds(), "submission_limit", cfg.UserOperationConcurrency)
				if err := telemetryService.Maintain(componentCtx); err != nil {
					maintenanceErr <- err
					return
				}
				certificateCtx, certificateSpan := otel.Tracer("ocservia.certificates").Start(componentCtx, "certificates.maintenance.run")
				if err := certificateService.Maintain(certificateCtx); err != nil {
					certificateSpan.RecordError(err)
					certificateSpan.SetStatus(codes.Error, "certificate maintenance failed")
					certificateSpan.End()
					logger.ErrorContext(componentCtx, "certificate maintenance failed", "alert_kind", "certificate.maintenance_failed", "error", err)
					maintenanceErr <- err
					return
				}
				certificateSpan.End()
				if err := auditManager.CheckpointAll(componentCtx); err != nil {
					maintenanceErr <- err
					return
				}
				select {
				case <-componentCtx.Done():
					maintenanceErr <- componentCtx.Err()
					return
				case <-ticker.C:
				}
			}
		}()
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
		case err := <-maintenanceErr:
			_ = stopTrust()
			return fmt.Errorf("run telemetry maintenance: %w", err)
		}
	}

	server := api.New(cfg.HTTPAddress, pool, api.BuildInfo{Version: build.Version, Commit: build.Commit, Role: string(cfg.Role)}, logger, cfg.BodyLimit, cfg.RequestTimeout, operationAuthEnabled(cfg), cfg.DevAuthToken, expectedSchemaVersion)
	var authService *auth.Service
	if cfg.OIDCEnabled() {
		authService, err = auth.New(ctx, pool, auth.Config{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.OIDCRedirectURL, SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL, BreakGlassEnabled: cfg.BreakGlassEnabled, BreakGlassTokenHash: cfg.BreakGlassTokenHash})
		if err != nil {
			return fmt.Errorf("configure OIDC: %w", err)
		}
	}
	server.EnableAuthorization(authService, rbac.New(pool), approvals.New(pool), auditManager)
	server.EnableOperations(operationService)
	server.EnableUserState(userStateService)
	server.EnableUserOperations(userOperationsService)
	server.EnableConfigPlans(configplan.New(pool, operationService))
	server.EnableCertificates(certificateService)
	server.EnableTelemetry(telemetryService)
	if cfg.ControllerEndpointID != "" {
		server.EnableEnrollment(enrollment.New(pool, cfg.ControllerEndpointID, build.Version), apiTransport)
	}
	server.EnableLocalSlice(sliceService)
	server.SetLocalSimulatorEnabled(cfg.LocalSimulator)
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
	case err := <-maintenanceErr:
		stopComponents()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		_ = stopTrust()
		return fmt.Errorf("run telemetry maintenance: %w", err)
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
