package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers handlers on the internal pprof listener only
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
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

func runRoles(ctx context.Context, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, expectedSchemaVersion int64, logger *slog.Logger) error {
	var err error
	logger.Info("control plane starting", "role", cfg.Role)
	if cfg.PprofAddress != "" {
		pprofServer := &http.Server{Addr: cfg.PprofAddress, Handler: http.DefaultServeMux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("pprof listener failed", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := pprofServer.Shutdown(shutdownCtx); err != nil {
				logger.Error("pprof shutdown failed", "error", err)
			}
		}()
		logger.Info("loopback pprof listener serving", "address", cfg.PprofAddress)
	}
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
	componentCtx, stopComponents := context.WithCancel(ctx)
	defer stopComponents()
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
	workerErr := make(chan error, 5)
	maintenanceErr := make(chan error, 1)
	var trust *trustserver.Server
	trustErr := make(chan error, 1)
	var workerTransport *transportclient.Client
	var ownerSessions *ownersession.Manager
	if cfg.RunsWorker() && (cfg.LocalSimulator || cfg.ControllerEndpointID != "") {
		workerTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return fmt.Errorf("configure transport client: %w", err)
		}
	}
	if cfg.RunsWorker() && cfg.ControllerEndpointID != "" {
		// The worker-role process is the per-node connection owner: it serves
		// session authorization and command dispatch, so its manager takes
		// the leases, signs fences, and pushes them to transportd.
		ownerSessions, err = ownersession.NewManagerBackend(backend, commandSigner, workerTransport, cfg.OwnerLeaseTTL, logger)
		if err != nil {
			return fmt.Errorf("configure connection owner sessions: %w", err)
		}
		go func() { workerErr <- ownerSessions.Run(componentCtx) }()
		// Transport disconnects, replacements, and revoke-driven closes end
		// the exact owner term behind the connection instead of letting a
		// live process keep renewing a session whose connection is gone.
		go func() { workerErr <- ownerSessions.WatchTransport(componentCtx, workerTransport) }()
		trust, err = trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.NewWithOwnerSessionsBackend(backend, cfg.ControllerEndpointID, build.Version, commandSigner, ownerSessions)), cfg.TransportUID)
		if err != nil {
			return fmt.Errorf("configure trust server: %w", err)
		}
		go func() { trustErr <- trust.Serve() }()
	}
	sliceService := localslice.NewBackend(backend, commandSigner)
	if ownerSessions != nil {
		sliceService = localslice.NewBackendWithCommandRecovery(backend, commandSigner, operationService, ownerSessions)
	}
	if cfg.TestResultCommitBarrier != "" {
		if err := sliceService.EnableResultCommitBarrier(cfg.TestResultCommitBarrier); err != nil {
			return fmt.Errorf("configure result commit barrier: %w", err)
		}
	}
	var fenceExecutor ownersession.FencedExecutor
	if ownerSessions != nil {
		fenceExecutor = ownerSessions
	}
	stopTrust := func() error {
		if trust == nil {
			return nil
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer shutdownCancel()
		return trust.Shutdown(shutdownCtx)
	}
	if workerTransport != nil {
		transport := workerTransport
		worker := localslice.NewWorker(sliceService, transport, logger)
		go func() { workerErr <- worker.Run(componentCtx) }()
		var operationWorker *operationstore.Worker
		operationWorker, err = operationstore.NewFencedWorker(operationService, transport, fenceExecutor, logger)
		if err != nil {
			return fmt.Errorf("configure outbox worker: %w", err)
		}
		if cfg.TestPreSendBarrier != "" {
			if err := operationWorker.EnablePreSendBarrier(cfg.TestPreSendBarrier, cfg.TestCommandLease); err != nil {
				return fmt.Errorf("configure command pre-send barrier: %w", err)
			}
		}
		go func() { workerErr <- operationWorker.Run(componentCtx) }()
		var trustWorker *enrollment.TrustConvergenceWorker
		trustWorker, err = enrollment.NewFencedTrustConvergenceWorkerBackend(backend, transport, fenceExecutor, logger)
		if err != nil {
			return fmt.Errorf("configure trust convergence worker: %w", err)
		}
		go func() { workerErr <- trustWorker.Run(componentCtx) }()
	}
	telemetryService := telemetrystore.NewWithRecommendedAgentVersionBackend(backend, cfg.RecommendedAgentVersion)
	telemetryService.EnableAgentUpgradeEligibility(releaseCatalog)
	userStateService := userstate.NewWithSignerBackend(backend, commandSigner)
	userOperationsService := useroperations.NewWithConcurrencyBackend(backend, userStateService, cfg.UserOperationConcurrency)
	var apiTransport *transportclient.Client
	if cfg.ControllerEndpointID != "" {
		apiTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return fmt.Errorf("configure API transport: %w", err)
		}
	}
	// Roles without the lease issue bindings for the fence transportd
	// registered, validated against the database ownership authority, so
	// administrative operations stay owner-fenced without a second lease
	// holder and a stale registered fence can never be re-signed.
	if fenceExecutor == nil && apiTransport != nil {
		observer, observerErr := ownersession.NewObserverBackend(backend, apiTransport, commandSigner)
		if observerErr != nil {
			return fmt.Errorf("configure owner fence observer: %w", observerErr)
		}
		fenceExecutor = observer
	}
	var certificateService *certificates.Service
	if cfg.CertificateSignerURL != "" {
		signer, signerErr := certificates.NewHTTPSigner(cfg.CertificateSignerURL, cfg.CertificateSignerToken, cfg.CertificateSignerTimeout)
		if signerErr != nil {
			return fmt.Errorf("configure external certificate signer: %w", signerErr)
		}
		certificateService = certificates.NewBackend(backend, operationService, signer, signer, apiTransport, commandSigner)
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
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			maintenanceErr <- runScheduler(componentCtx, leader, ticker.C, work, cfg.UserOperationConcurrency, logger)
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

	server := api.NewBackend(cfg.HTTPAddress, backend, api.BuildInfo{Version: build.Version, Commit: build.Commit, Role: string(cfg.Role), RecommendedAgentVersion: cfg.RecommendedAgentVersion}, logger, cfg.BodyLimit, cfg.RequestTimeout, operationAuthEnabled(cfg), cfg.DevAuthToken, expectedSchemaVersion)
	server.EnableBrowserOrigin(cfg.BrowserOrigin())
	server.ConfigureAuthProxies(cfg.AuthTrustedProxyCIDRs)
	if err := server.ConfigureEventStreams(cfg.EventStreams); err != nil {
		return fmt.Errorf("configure SSE admission: %w", err)
	}
	var authService *auth.Service
	if cfg.LocalAuthEnabled() || cfg.OIDCEnabled() {
		authConfig := auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL, BreakGlassEnabled: cfg.BreakGlassEnabled, BreakGlassTokenHash: cfg.BreakGlassTokenHash}
		if cfg.OIDCEnabled() {
			authConfig.Issuer, authConfig.ClientID = cfg.OIDCIssuer, cfg.OIDCClientID
			authConfig.ClientSecret, authConfig.RedirectURL = cfg.OIDCClientSecret, cfg.OIDCRedirectURL
		}
		authService, err = auth.NewBackend(backend, authConfig)
		if err != nil {
			return fmt.Errorf("configure authentication: %w", err)
		}
	}
	server.EnableAuthorization(authService, rbac.NewBackend(backend), approvals.NewBackend(backend), auditManager)
	server.EnableOperations(operationService)
	server.EnableReleaseCatalog(releaseCatalog)
	server.EnableUserState(userStateService)
	server.EnableUserOperations(userOperationsService)
	server.EnableConfigPlans(configplan.NewBackend(backend, operationService))
	server.EnableCertificates(certificateService)
	server.EnablePrivdAttestation(privdattestation.NewBackend(backend))
	server.EnableTelemetry(telemetryService)
	if cfg.ControllerEndpointID != "" {
		server.EnableEnrollment(enrollment.NewBackend(backend, cfg.ControllerEndpointID, build.Version, commandSigner), apiTransport)
		server.EnableOwnerFencing(fenceExecutor)
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
