package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/devsim"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/GentleKingson/ocservia/control-plane/internal/trustserver"
)

func startWorker(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, commandSigner *commandauth.Signer, operationService *operations.Service, logger *slog.Logger) (*transportevents.Service, *devsim.Service, func(), error) {
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
			return nil, nil, cleanup, fmt.Errorf("configure transport client: %w", err)
		}
	}
	if cfg.ControllerEndpointID != "" {
		ownerSessions, err = ownersession.NewManager(backend, logger)
		if err != nil {

			return nil, nil, cleanup, fmt.Errorf("configure connection owner sessions: %w", err)
		}
		life.start("watch owner transport", func(ctx context.Context) error { return ownerSessions.WatchTransport(ctx, workerTransport) })
		trust, err := trustserver.New(cfg.TrustSocket, trustserver.NewHandler(enrollment.NewWithOwnerSessions(backend, cfg.ControllerEndpointID, build.Version, commandSigner, ownerSessions), backend), cfg.TransportUID)
		if err != nil {

			return nil, nil, cleanup, fmt.Errorf("configure trust server: %w", err)
		}
		life.trust = trust
		life.start("serve trust UDS", func(context.Context) error { return trust.Serve() })
	}
	eventService := transportevents.New(backend, commandSigner)
	if ownerSessions != nil {
		eventService = transportevents.NewWithCommandRecovery(backend, commandSigner, operationService, ownerSessions)
	}
	var simService *devsim.Service
	if cfg.LocalSimulator {
		simService = devsim.New(backend)
		eventService.SetHooks(simService.HandleIngress, simService.PrepareEventGap)
	}
	if workerTransport != nil {
		transport := workerTransport
		life.start("run transport event worker", func(ctx context.Context) error {
			return transport.RunWatch(ctx, eventService, eventService)
		})
		if simService != nil {
			life.start("run simulator dispatch worker", devsim.NewWorker(simService, transport, logger).Run)
		}
		operationWorker, err := operations.NewWorker(operationService, transport, logger)
		if err != nil {

			return nil, nil, cleanup, fmt.Errorf("configure outbox worker: %w", err)
		}
		life.start("run outbox worker", operationWorker.Run)
		trustWorker, err := enrollment.NewTrustConvergenceWorker(backend, transport, logger)
		if err != nil {

			return nil, nil, cleanup, fmt.Errorf("configure trust convergence worker: %w", err)
		}
		life.start("run trust convergence worker", trustWorker.Run)
	}

	return eventService, simService, cleanup, nil
}
