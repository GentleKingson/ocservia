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
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
)

type BuildInfo struct{ Version, Commit string }

func Run(ctx context.Context, cfg config.Config, build BuildInfo, logger *slog.Logger) error {
	if err := validateBootstrapPasswords(cfg); err != nil {
		return err
	}
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

	conn, err := connection.Open(ctx, connection.Options{Backend: cfg.DatabaseBackend, Environment: cfg.Environment, URL: cfg.DatabaseURL, CAFile: cfg.DatabaseTLSCAFile})
	if err != nil {
		return err
	}
	defer conn.Close()
	backend := conn.Store
	startup, err := initializeDatabase(ctx, conn, cfg, logger)
	if err != nil || startup == nil {
		return err
	}
	return runRoles(ctx, cfg, build, backend, startup.audit, startup.schemaVersion, logger)
}

func runRoles(ctx context.Context, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, expectedSchemaVersion int64, logger *slog.Logger) (runErr error) {
	life := newLifecycle(ctx, cfg.ShutdownTimeout, logger)
	stopPprof := startPprof(cfg.PprofAddress, logger)
	defer func() {
		runErr = life.close(runErr)
		stopPprof()
		if cfg.RunsAPI() && errors.Is(runErr, context.Canceled) {
			logger.Info("control plane stopped", "role", cfg.Role)
		}
	}()
	var err error
	logger.Info("control plane starting", "role", cfg.Role)
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
	operationService := operationstore.NewBackend(backend, cfg.UserOperationConcurrency, commandSigner)
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
	sliceService, fenceExecutor, err := startWorker(life, cfg, build, backend, commandSigner, operationService, logger)
	if err != nil {
		return err
	}
	telemetryService := telemetrystore.NewWithRecommendedAgentVersionBackend(backend, cfg.RecommendedAgentVersion)
	telemetryService.EnableAgentUpgradeEligibility(releaseCatalog)
	userStateService := userstate.NewWithSignerBackend(backend, commandSigner)
	userOperationsService := useroperations.NewWithConcurrencyBackend(backend, userStateService, cfg.UserOperationConcurrency)
	var controlTransport *transportclient.Client
	if cfg.ControllerEndpointID != "" {
		controlTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return fmt.Errorf("configure API transport: %w", err)
		}
	}
	// Roles without the lease issue bindings for the fence transportd
	// registered, validated against the database ownership authority, so
	// administrative operations stay owner-fenced without a second lease
	// holder and a stale registered fence can never be re-signed.
	if fenceExecutor == nil && controlTransport != nil {
		observer, observerErr := ownersession.NewObserverBackend(backend, controlTransport, commandSigner)
		if observerErr != nil {
			return fmt.Errorf("configure owner fence observer: %w", observerErr)
		}
		fenceExecutor = observer
	}
	var certificateService *certificates.Service
	if cfg.CertificateSignerURL != "" {
		signer, signerErr := certificates.NewHTTPSignerWithCA(cfg.CertificateSignerURL, cfg.CertificateSignerToken, cfg.CertificateSignerTimeout, cfg.CertificateSignerCAFile)
		if signerErr != nil {
			return fmt.Errorf("configure external certificate signer: %w", signerErr)
		}
		certificateService = certificates.NewBackend(backend, operationService, signer, signer, controlTransport, commandSigner)
	} else {
		certificateService = certificates.NewBackend(backend, operationService, nil, nil, nil, nil)
	}
	certificateService.EnableOwnerFencing(fenceExecutor)
	if cfg.RunsScheduler() {
		identity, identityErr := coordination.NewIdentity()
		if identityErr != nil {
			return fmt.Errorf("mint scheduler identity: %w", identityErr)
		}
		// The leadership lease spans the whole maintenance session and is
		// renewed in the background; losing renewal cancels the session
		// context, which aborts fenced transactions before they can commit.
		leader := coordination.NewRunnerBackend(backend, identity, 15*time.Second, 5*time.Second, logger)

		work := maintenanceWork{
			users:        userOperationsService.RunOnce,
			rollouts:     operationService.AdvanceAgentRollouts,
			telemetry:    telemetryService.Maintain,
			certificates: certificateService.Maintain,
			audit:        auditManager.CheckpointAll,
		}
		if cfg.TestSchedulerEvidence {
			work.evidence = func(ctx context.Context, session *coordination.Session) error {
				return coordination.RecordMaintenanceCompletion(ctx, backend, session)
			}
		}
		life.start("run telemetry maintenance", func(ctx context.Context) error {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			return runScheduler(ctx, leader, ticker.C, work, cfg.UserOperationConcurrency, logger)
		})
	}
	if !cfg.RunsAPI() {
		return life.wait()
	}

	services := httpServices{
		modules:    api.Modules{Nodes: telemetryService, ConfigPlans: configplan.NewBackend(backend, operationService), UserOperations: userOperationsService, Certificates: certificateService},
		operations: operationService, releaseCatalog: releaseCatalog, userState: userStateService, localSlice: sliceService,
	}
	if cfg.ControllerEndpointID != "" {
		services.enrollment = enrollment.NewBackend(backend, cfg.ControllerEndpointID, build.Version, commandSigner)
		services.transport, services.fences = controlTransport, fenceExecutor
	}
	server, err := newHTTPServer(life, cfg, build, backend, auditManager, expectedSchemaVersion, logger, services)
	if err != nil {
		return err
	}
	life.start("serve HTTP", func(context.Context) error { return server.ListenAndServe() })
	return life.wait()
}
