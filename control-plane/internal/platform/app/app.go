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
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
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
		auditManager, err := newAuditManager(pool, cfg)
		if err != nil {
			return err
		}
		if err := migrations.Migrate(databaseCtx, pool, auditManager.PreflightAuthenticityMigration); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
		if err := auditManager.EnsureAuthenticity(databaseCtx); err != nil {
			return fmt.Errorf("transition audit event authentication: %w", err)
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
	auditManager, err := newAuditManager(pool, cfg)
	if err != nil {
		return err
	}
	if err := auditManager.EnsureAuthenticity(databaseCtx); err != nil {
		return fmt.Errorf("verify audit event authentication: %w", err)
	}

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
	operationService := operationstore.NewWithSigner(pool, cfg.UserOperationConcurrency, commandSigner)
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
		ownerSessions, err = ownersession.NewManager(pool, commandSigner, workerTransport, cfg.OwnerLeaseTTL, logger)
		if err != nil {
			return fmt.Errorf("configure connection owner sessions: %w", err)
		}
		go func() { workerErr <- ownerSessions.Run(componentCtx) }()
		// Transport disconnects, replacements, and revoke-driven closes end
		// the exact owner term behind the connection instead of letting a
		// live process keep renewing a session whose connection is gone.
		go func() { workerErr <- ownerSessions.WatchTransport(componentCtx, workerTransport) }()
		trust, err = trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.NewWithOwnerSessions(pool, cfg.ControllerEndpointID, build.Version, commandSigner, ownerSessions)), cfg.TransportUID)
		if err != nil {
			return fmt.Errorf("configure trust server: %w", err)
		}
		go func() { trustErr <- trust.Serve() }()
	}
	sliceService := localslice.NewWithSigner(pool, commandSigner)
	if ownerSessions != nil {
		sliceService = localslice.NewWithCommandRecovery(pool, commandSigner, operationService, ownerSessions)
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
		trustWorker, err = enrollment.NewFencedTrustConvergenceWorker(pool, transport, fenceExecutor, logger)
		if err != nil {
			return fmt.Errorf("configure trust convergence worker: %w", err)
		}
		go func() { workerErr <- trustWorker.Run(componentCtx) }()
	}
	telemetryService := telemetrystore.New(pool)
	userStateService := userstate.NewWithSigner(pool, commandSigner)
	userOperationsService := useroperations.NewWithConcurrency(pool, userStateService, cfg.UserOperationConcurrency)
	var apiTransport *transportclient.Client
	if cfg.ControllerEndpointID != "" {
		apiTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return fmt.Errorf("configure API transport: %w", err)
		}
	}
	// Roles without the lease issue bindings for the fence transportd
	// registered, validated against the PostgreSQL ownership authority, so
	// administrative operations stay owner-fenced without a second lease
	// holder and a stale registered fence can never be re-signed.
	if fenceExecutor == nil && apiTransport != nil {
		observer, observerErr := ownersession.NewObserver(pool, apiTransport, commandSigner)
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
		certificateService = certificates.NewWithDependencies(pool, operationService, signer, signer, apiTransport, commandSigner)
	} else {
		certificateService = certificates.New(pool, operationService)
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
		leader := coordination.NewRunner(pool, identity, 15*time.Second, 5*time.Second, logger)
		go func() {
			defer leader.Stop()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				started := time.Now()
				err := leader.WithSession(componentCtx, func(sessionCtx context.Context, session *coordination.Session) error {
					runCtx, span := otel.Tracer("ocservia.useroperations").Start(sessionCtx, "user_operations.scheduler.run")
					if err := userOperationsService.RunOnce(runCtx); err != nil {
						span.RecordError(err)
						span.SetStatus(codes.Error, "scheduler run failed")
						span.End()
						return err
					}
					span.End()
					logger.InfoContext(sessionCtx, "user operations scheduler completed", "duration_ms", time.Since(started).Milliseconds(), "submission_limit", cfg.UserOperationConcurrency)
					if err := telemetryService.Maintain(sessionCtx); err != nil {
						return err
					}
					certificateCtx, certificateSpan := otel.Tracer("ocservia.certificates").Start(sessionCtx, "certificates.maintenance.run")
					if err := certificateService.Maintain(certificateCtx); err != nil {
						certificateSpan.RecordError(err)
						certificateSpan.SetStatus(codes.Error, "certificate maintenance failed")
						certificateSpan.End()
						return err
					}
					certificateSpan.End()
					if err := auditManager.CheckpointAll(sessionCtx); err != nil {
						return err
					}
					if cfg.TestSchedulerEvidence {
						if err := coordination.RecordMaintenanceCompletion(sessionCtx, pool, session); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					// Leadership loss is expected during failover: stay
					// alive, stop scheduling, and retry on the next tick. A
					// cancellation while the component context is still live
					// means the session context was cancelled by renewal
					// loss, not a shutdown.
					leadershipLost := errors.Is(err, coordination.ErrLeadershipLost) ||
						errors.Is(err, coordination.ErrNotLeader) ||
						(componentCtx.Err() == nil && errors.Is(err, context.Canceled))
					if leadershipLost {
						logger.WarnContext(componentCtx, "maintenance session lost leadership", "alert_kind", "scheduler.leadership_lost", "error", err)
					} else {
						logger.ErrorContext(componentCtx, "user operations scheduler failed", "alert_kind", "user_operations.scheduler_failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
						maintenanceErr <- err
						return
					}
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
	if err := server.ConfigureEventStreams(cfg.EventStreams); err != nil {
		return fmt.Errorf("configure SSE admission: %w", err)
	}
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
	server.EnablePrivdAttestation(privdattestation.New(pool))
	server.EnableTelemetry(telemetryService)
	if cfg.ControllerEndpointID != "" {
		server.EnableEnrollment(enrollment.New(pool, cfg.ControllerEndpointID, build.Version, commandSigner), apiTransport)
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

func newAuditManager(pool *pgxpool.Pool, cfg config.Config) (*audit.Manager, error) {
	if len(cfg.AuditEventKey) == 0 {
		return audit.NewManager(pool, cfg.AuditCheckpointKey), nil
	}
	manager, err := audit.NewManagerWithEventKey(pool, cfg.AuditCheckpointKey, cfg.AuditEventKeyID, cfg.AuditEventKey)
	if err != nil {
		return nil, fmt.Errorf("configure audit event authentication: %w", err)
	}
	return manager, nil
}

func operationAuthEnabled(cfg config.Config) bool {
	return cfg.DevAuth
}
