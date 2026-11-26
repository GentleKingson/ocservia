package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	platformtelemetry "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
)

type BuildInfo struct{ Version, Commit string }

func Run(ctx context.Context, cfg config.Config, build BuildInfo, logger, accessLogger *slog.Logger, shutdownDeadline func() time.Time) error {
	if err := validateBootstrapPasswords(cfg); err != nil {
		return err
	}
	shutdownTelemetry, err := platformtelemetry.Configure(ctx, cfg.OTLPEndpoint, build.Version, cfg.Environment)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithDeadline(context.Background(), shutdownDeadline())
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()

	conn, err := connection.Open(ctx, connection.Options{Environment: cfg.Environment, URL: cfg.DatabaseURL, CAFile: cfg.DatabaseTLSCAFile, BundledPostgres: cfg.DatabaseBundledPostgres})
	if err != nil {
		return err
	}
	defer conn.Close()
	if cfg.MigrateOnly || cfg.ProvisionWorkspace || cfg.BootstrapLocalAdmin || cfg.TelemetryRetentionOnly {
		_, err := initializeDatabase(ctx, conn, cfg, logger)
		return err
	}
	monitorLock, releaseLock, err := conn.AcquireControllerLock(ctx)
	if err != nil {
		return err
	}
	defer releaseLock()
	runCtx, stop := context.WithCancel(ctx)
	lockResult := make(chan error, 1)
	go func() {
		err := monitorLock(runCtx)
		if err != nil {
			stop()
		}
		lockResult <- err
	}()
	auditManager, runErr := initializeDatabase(runCtx, conn, cfg, logger)
	if runErr == nil {
		runErr = runController(runCtx, cfg, build, conn.Store, auditManager, logger, accessLogger, shutdownDeadline)
	}
	stop()
	if lockErr := <-lockResult; lockErr != nil {
		return errors.Join(lockErr, failureCause(runErr))
	}
	return runErr
}

func runController(ctx context.Context, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, logger, accessLogger *slog.Logger, shutdownDeadline func() time.Time) (runErr error) {
	life := newLifecycle(ctx, shutdownDeadline, logger)
	stopPprof := startPprof(cfg.PprofAddress, logger)
	var closeWorkerTransport func()
	var controlTransport *transportclient.Client
	defer func() {
		runErr = life.close(runErr)
		if controlTransport != nil {
			_ = controlTransport.Close()
		}
		if closeWorkerTransport != nil {
			closeWorkerTransport()
		}
		stopPprof()
		if errors.Is(runErr, context.Canceled) {
			logger.Info("control plane stopped")
		}
	}()
	var err error
	logger.Info("control plane starting")
	var commandSigner *commandauth.Signer
	if cfg.CommandSigningKeyFile != "" {
		commandSigner, err = commandauth.LoadSigner(cfg.CommandSigningKeyFile)
		if err != nil {
			return fmt.Errorf("load Controller command signing key: %w", err)
		}
	} else {
		commandSigner, err = commandauth.NewRandomSigner()
		if err != nil {
			return err
		}
		logger.Warn("using an ephemeral Controller command signing key", "environment", cfg.Environment)
	}
	operationService := operations.New(backend, cfg.UserOperationConcurrency, commandSigner)
	if err := operationService.SetAgentUpgradeReconcileTimeout(cfg.AgentUpgradeReconcile); err != nil {
		return fmt.Errorf("configure agent upgrade reconciliation window: %w", err)
	}
	var releaseCatalog *releasecatalog.Catalog
	if cfg.AgentReleaseManifest != "" {
		releaseCatalog, err = releasecatalog.Load(cfg.AgentReleaseManifest)
		if err != nil {
			return fmt.Errorf("load trusted agent release manifest: %w", err)
		}
	}
	operationService.EnableReleaseCatalog(releaseCatalog)
	eventService, closeWorkerTransport, err := startWorker(life, cfg, build, backend, commandSigner, operationService, logger)
	if err != nil {
		return err
	}
	telemetryService := telemetry.NewWithRecommendedAgentVersion(backend, cfg.RecommendedAgentVersion)
	telemetryService.EnableAgentUpgradeEligibility(releaseCatalog)
	userStateService := userstate.NewWithOperations(backend, commandSigner, operationService)
	userOperationsService := useroperations.NewWithConcurrency(backend, userStateService, cfg.UserOperationConcurrency)
	if cfg.ControllerEndpointID != "" {
		controlTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return fmt.Errorf("configure API transport: %w", err)
		}
	}
	var certificateService *certificates.Service
	var publicKeySigner *certificates.HTTPSigner
	if cfg.CertificateSignerURL != "" {
		signer, signerErr := certificates.NewHTTPSignerWithCA(cfg.CertificateSignerURL, cfg.CertificateSignerToken, cfg.CertificateSignerTimeout, cfg.CertificateSignerCAFile)
		if signerErr != nil {
			return fmt.Errorf("configure external certificate signer: %w", signerErr)
		}
		publicKeySigner = signer
		certificateService = certificates.New(backend, operationService, signer, signer, controlTransport, commandSigner)
	} else {
		certificateService = certificates.New(backend, operationService, nil, nil, nil, nil)
	}

	history, err := historyretention.New(backend, cfg.HistoryRetention)
	if err != nil {
		return err
	}
	work := maintenanceWork{
		users:        userOperationsService.RunOnce,
		rollouts:     operationService.AdvanceAgentRollouts,
		telemetry:    telemetryService.Maintain,
		certificates: certificateService.Maintain,
		audit:        auditManager.CheckpointAll,
		retention: func(ctx context.Context) error {
			if err := history.RunOnce(ctx); err != nil {
				return err
			}
			return auditManager.CompactDetails(ctx, cfg.HistoryRetention.AuditDays)
		},
	}
	life.start("run telemetry maintenance", func(ctx context.Context) error {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		return runScheduler(ctx, ticker.C, work, cfg.UserOperationConcurrency, logger)
	})

	modules := api.Modules{
		AccessLogger: accessLogger,
		Nodes:        telemetryService, ConfigPlans: configplan.New(backend, operationService), UserOperations: userOperationsService, Certificates: certificateService,
		Operations: operationService, ReleaseCatalog: releaseCatalog, UserState: userStateService, TransportEvents: eventService,
	}
	if cfg.ControllerEndpointID != "" {
		modules.Enrollment = enrollment.New(backend, cfg.ControllerEndpointID, build.Version, commandSigner)
		if publicKeySigner != nil {
			modules.Enrollment.EnableUserPasswordPublicKeys(publicKeySigner)
		}
		modules.Transport = controlTransport
	}
	server, err := newHTTPServer(life, cfg, build, backend, auditManager, logger, modules)
	if err != nil {
		return err
	}
	life.start("serve HTTP", func(context.Context) error { return server.ListenAndServe() })
	return life.wait()
}
