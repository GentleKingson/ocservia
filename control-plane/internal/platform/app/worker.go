package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
)

func startWorker(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, commandSigner *commandauth.Signer, operationService *operationstore.Service, logger *slog.Logger) (*localslice.Service, ownersession.FencedExecutor, error) {
	var err error
	var workerTransport *transportclient.Client
	var ownerSessions *ownersession.Manager
	if cfg.RunsWorker() && (cfg.LocalSimulator || cfg.ControllerEndpointID != "") {
		workerTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return nil, nil, fmt.Errorf("configure transport client: %w", err)
		}
	}
	if cfg.RunsWorker() && cfg.ControllerEndpointID != "" {
		// The worker-role process is the per-node connection owner: it serves
		// session authorization and command dispatch, so its manager takes
		// the leases, signs fences, and pushes them to transportd.
		ownerSessions, err = ownersession.NewManagerBackend(backend, commandSigner, workerTransport, cfg.OwnerLeaseTTL, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("configure connection owner sessions: %w", err)
		}
		life.start("run owner sessions", ownerSessions.Run)
		// Transport disconnects, replacements, and revoke-driven closes end
		// the exact owner term behind the connection instead of letting a
		// live process keep renewing a session whose connection is gone.
		life.start("watch owner transport", func(ctx context.Context) error { return ownerSessions.WatchTransport(ctx, workerTransport) })
		trust, err := trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.NewWithOwnerSessionsBackend(backend, cfg.ControllerEndpointID, build.Version, commandSigner, ownerSessions)), cfg.TransportUID)
		if err != nil {
			return nil, nil, fmt.Errorf("configure trust server: %w", err)
		}
		life.trust = trust
		life.start("serve trust UDS", func(context.Context) error { return trust.Serve() })
	}
	sliceService := localslice.NewBackend(backend, commandSigner)
	if ownerSessions != nil {
		sliceService = localslice.NewBackendWithCommandRecovery(backend, commandSigner, operationService, ownerSessions)
	}
	if cfg.TestResultCommitBarrier != "" {
		if err := sliceService.EnableResultCommitBarrier(cfg.TestResultCommitBarrier); err != nil {
			return nil, nil, fmt.Errorf("configure result commit barrier: %w", err)
		}
	}
	var fenceExecutor ownersession.FencedExecutor
	if ownerSessions != nil {
		fenceExecutor = ownerSessions
	}
	if workerTransport != nil {
		transport := workerTransport
		worker := localslice.NewWorker(sliceService, transport, logger)
		life.start("run local slice worker", worker.Run)
		var operationWorker *operationstore.Worker
		operationWorker, err = operationstore.NewFencedWorker(operationService, transport, fenceExecutor, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("configure outbox worker: %w", err)
		}
		if cfg.TestPreSendBarrier != "" {
			if err := operationWorker.EnablePreSendBarrier(cfg.TestPreSendBarrier, cfg.TestCommandLease); err != nil {
				return nil, nil, fmt.Errorf("configure command pre-send barrier: %w", err)
			}
		}
		life.start("run outbox worker", operationWorker.Run)
		var trustWorker *enrollment.TrustConvergenceWorker
		trustWorker, err = enrollment.NewFencedTrustConvergenceWorkerBackend(backend, transport, fenceExecutor, logger)
		if err != nil {
			return nil, nil, fmt.Errorf("configure trust convergence worker: %w", err)
		}
		life.start("run trust convergence worker", trustWorker.Run)
	}

	return sliceService, fenceExecutor, nil
}
