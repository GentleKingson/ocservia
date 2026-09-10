package localslice

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	localstore "github.com/GentleKingson/ocservia/control-plane/internal/localslice/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func (s *Service) Create(ctx context.Context, scenario Scenario, requestID, traceparent string) (Operation, error) {
	ctx, span := otel.Tracer("ocservia.localslice").Start(ctx, "local_slice.queue", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	heartbeats, delay, err := normalizeScenario(scenario)
	if err != nil {
		return Operation{}, err
	}
	if requestID == "" || !validTraceparent(traceparent) {
		return Operation{}, fmt.Errorf("%w: request correlation is required", ErrInvalidScenario)
	}
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return Operation{}, err
	}
	expires, err := at.Add(time.Minute)
	if err != nil {
		return Operation{}, err
	}
	workspace, node, operation, command, auditID, err := newIDs()
	if err != nil {
		return Operation{}, err
	}
	envelope, err := marshalEnvelope(node, operation, command, traceparent, scenario, heartbeats, delay, now)
	if err != nil {
		return Operation{}, err
	}
	endpoint := sha256.Sum256(append([]byte("ocservia/development-simulator/"), node[:]...))
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		workspace, err = store.EnsureWorkspace(ctx, workspace, workspaceSlug, at)
		if err != nil {
			return fmt.Errorf("ensure simulator workspace: %w", err)
		}
		if err := store.InsertSimulation(ctx, localstore.Simulation{WorkspaceID: workspace, NodeID: node, OperationID: operation, CommandID: command, Endpoint: endpoint[:], Envelope: envelope, RequestID: requestID, TraceID: traceID(traceparent), Traceparent: traceparent, At: at, ExpiresAt: expires}); err != nil {
			return fmt.Errorf("insert simulator intent: %w", err)
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspace, ActorType: "development_stub", ActorID: "developer", Action: "simulation.probe", ResourceType: "operation", ResourceID: operation, RequestID: requestID, TraceID: traceID(traceparent), Reason: "I03 local side-effect-free slice", At: now})
	})
	if err != nil {
		return Operation{}, fmt.Errorf("create local simulation: %w", err)
	}
	nodeText, commandText := node.String(), command.String()
	return Operation{ID: operation.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: at, UpdatedAt: at}, nil
}

func (s *Service) ClaimJobs(ctx context.Context, limit int) ([]Job, error) {
	var jobs []Job
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		jobs, err = store.ClaimJobs(ctx, limit)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim simulator jobs: %w", err)
	}
	return jobs, nil
}

func (s *Service) ExpireJobs(ctx context.Context) error {
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		return store.ExpireJobs(ctx)
	})
	if err != nil {
		return fmt.Errorf("expire simulator jobs: %w", err)
	}
	return nil
}

func (s *Service) MarkDispatchStarted(ctx context.Context, id uuid.UUID) error {
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		changed, err := store.MarkDispatchStarted(ctx, id)
		if err != nil {
			return fmt.Errorf("mark simulator dispatch started: %w", err)
		}
		if !changed {
			return errors.New("simulator job is no longer dispatchable")
		}
		return nil
	})
}

func (s *Service) MarkDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	if len(message) > 512 {
		message = message[:512]
	}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := localstore.Local(tx)
		if err != nil {
			return err
		}
		return store.MarkDispatchError(ctx, id, message)
	})
	if err != nil {
		return fmt.Errorf("record simulator dispatch error: %w", err)
	}
	return nil
}
