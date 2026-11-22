package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
)

func startWorker(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, commandSigner *commandauth.Signer, operationService *operations.Service, logger *slog.Logger) (*transportevents.Service, func(), error) {
	var err error
	var workerTransport *transportclient.Client
	var ownerSessions *ownersession.Manager
	cleanup := func() {
		if workerTransport != nil {
			_ = workerTransport.Close()
		}
	}
	if cfg.LocalSimulator || cfg.ControllerEndpointID != "" {
		workerTransport, err = transportclient.New(cfg.TransportSocket, cfg.TransportTimeout, cfg.TransportQueue, cfg.TransportUID, cfg.TransportGID)
		if err != nil {
			return nil, cleanup, fmt.Errorf("configure transport client: %w", err)
		}
	}
	if cfg.ControllerEndpointID != "" {
		ownerSessions, err = ownersession.NewManagerBackend(backend, logger)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("configure connection owner sessions: %w", err)
		}
		life.start("watch owner transport", func(ctx context.Context) error { return ownerSessions.WatchTransport(ctx, workerTransport) })
		trust, err := trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.NewWithOwnerSessionsBackend(backend, cfg.ControllerEndpointID, build.Version, commandSigner, ownerSessions), backend), cfg.TransportUID)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("configure trust server: %w", err)
		}
		life.trust = trust
		life.start("serve trust UDS", func(context.Context) error { return trust.Serve() })
	}
	eventService := transportevents.NewBackend(backend, commandSigner)
	if ownerSessions != nil {
		eventService = transportevents.NewBackendWithCommandRecovery(backend, commandSigner, operationService, ownerSessions)
	}
	eventService.SetLocalSimulator(cfg.LocalSimulator)
	if workerTransport != nil {
		transport := workerTransport
		worker := transportevents.NewWorker(eventService, transport, logger, cfg.LocalSimulator)
		life.start("run transport event worker", worker.Run)
		operationWorker, err := operations.NewWorker(operationService, transport, logger)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("configure outbox worker: %w", err)
		}
		life.start("run outbox worker", operationWorker.Run)
		trustWorker, err := enrollment.NewTrustConvergenceWorkerBackend(backend, transport, logger)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("configure trust convergence worker: %w", err)
		}
		life.start("run trust convergence worker", trustWorker.Run)
	}

	return eventService, cleanup, nil
}
