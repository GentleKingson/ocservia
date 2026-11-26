package devsim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	devsimstore "github.com/GentleKingson/ocservia/control-plane/internal/devsim/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const workspaceSlug = "local-simulator"

var ErrInvalidScenario = errors.New("invalid simulation")

type SimulationScenario struct {
	HeartbeatCount  *uint32 `json:"heartbeat_count"`
	DelayMillis     *uint32 `json:"delay_millis"`
	DuplicateEvent  bool    `json:"duplicate_event"`
	ReturnError     bool    `json:"return_error"`
	DisconnectAfter bool    `json:"disconnect_after"`
}

type SimulationJob = devsimstore.SimulationJob

type Service struct {
	backend database.Backend
	now     func() time.Time
}

func New(backend database.Backend) *Service {
	return &Service{backend: backend, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) CreateSimulation(ctx context.Context, scenario SimulationScenario, requestID, traceparent string) (operations.Operation, error) {
	ctx, span := otel.Tracer("ocservia.devsim").Start(ctx, "development.simulation.queue", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	heartbeats, delay, err := normalizeScenario(scenario)
	if err != nil {
		return operations.Operation{}, err
	}
	if requestID == "" || tracecontext.TraceID(traceparent) == "" {
		return operations.Operation{}, fmt.Errorf("%w: request correlation is required", ErrInvalidScenario)
	}
	now := s.now().UTC()
	expires := now.Add(time.Minute)
	workspace, node, operation, command, auditID, err := newIDs()
	if err != nil {
		return operations.Operation{}, err
	}
	envelope, err := marshalEnvelope(node, operation, command, traceparent, scenario, heartbeats, delay, now)
	if err != nil {
		return operations.Operation{}, err
	}
	endpoint := sha256.Sum256(append([]byte("ocservia/development-simulator/"), node[:]...))
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store := devsimstore.FromTransaction(tx)
		workspace, err = store.EnsureWorkspace(ctx, workspace, workspaceSlug, now)
		if err != nil {
			return fmt.Errorf("ensure simulator workspace: %w", err)
		}
		if err := store.InsertSimulation(ctx, devsimstore.Simulation{WorkspaceID: workspace, NodeID: node, OperationID: operation, CommandID: command, Endpoint: endpoint[:], Envelope: envelope, RequestID: requestID, TraceID: tracecontext.TraceID(traceparent), Traceparent: traceparent, At: now, ExpiresAt: expires}); err != nil {
			return fmt.Errorf("insert simulator intent: %w", err)
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspace, ActorType: "development_stub", ActorID: "developer", Action: "simulation.probe", ResourceType: "operation", ResourceID: operation, RequestID: requestID, TraceID: tracecontext.TraceID(traceparent), Reason: "development simulation probe", At: now})
	})
	if err != nil {
		return operations.Operation{}, fmt.Errorf("create local simulation: %w", err)
	}
	nodeText, commandText := node.String(), command.String()
	return operations.Operation{ID: operation.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Service) ClaimSimulationJobs(ctx context.Context, limit int) ([]SimulationJob, error) {
	var jobs []SimulationJob
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		jobs, err = devsimstore.FromTransaction(tx).ClaimSimulationJobs(ctx, limit)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim simulator jobs: %w", err)
	}
	return jobs, nil
}

func (s *Service) ExpireSimulationJobs(ctx context.Context) error {
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		return devsimstore.FromTransaction(tx).ExpireSimulationJobs(ctx)
	})
	if err != nil {
		return fmt.Errorf("expire simulator jobs: %w", err)
	}
	return nil
}

func (s *Service) MarkSimulationDispatchStarted(ctx context.Context, id uuid.UUID) error {
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		changed, err := devsimstore.FromTransaction(tx).MarkSimulationDispatchStarted(ctx, id)
		if err != nil {
			return fmt.Errorf("mark simulator dispatch started: %w", err)
		}
		if !changed {
			return errors.New("simulator job is no longer dispatchable")
		}
		return nil
	})
}

func (s *Service) MarkSimulationDispatchError(ctx context.Context, id uuid.UUID, message string) error {
	if len(message) > 512 {
		message = message[:512]
	}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		return devsimstore.FromTransaction(tx).MarkSimulationDispatchError(ctx, id, message)
	})
	if err != nil {
		return fmt.Errorf("record simulator dispatch error: %w", err)
	}
	return nil
}

func (s *Service) HandleIngress(ctx context.Context, tx database.Tx, nodeID uuid.UUID, eventType, traceparent string, occurredAt time.Time) error {
	if eventType == "telemetry" || eventType == "path_changed" || eventType == "command_result" {
		return nil
	}
	operationState := "running"
	switch eventType {
	case "simulation_result":
		operationState = "succeeded"
	case "error":
		operationState = "failed"
	case "disconnected":
		operationState = "unknown"
	}
	if err := devsimstore.FromTransaction(tx).SimulationOutcome(ctx, nodeID, operationState, traceparent, eventType == "disconnected", occurredAt); err != nil {
		return fmt.Errorf("update operation from transport event: %w", err)
	}
	return nil
}

func (s *Service) PrepareEventGap(ctx context.Context, readConnection transportclient.NodeConnectionReader) (func(context.Context, database.Tx) error, error) {
	var candidates []devsimstore.GapNode
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) (err error) {
		candidates, err = devsimstore.FromTransaction(tx).GapNodes(ctx, workspaceSlug)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("select simulator nodes after transport event gap: %w", err)
	}
	var disconnected []devsimstore.GapNode
	// No transaction is held while consulting the external transport registry.
	for _, node := range candidates {
		conn, err := readConnection(ctx, node.ID[:])
		if err != nil {
			return nil, fmt.Errorf("reconcile simulator node connection %s: %w", node.ID, err)
		}
		if conn == nil {
			disconnected = append(disconnected, node)
		}
	}
	return func(ctx context.Context, tx database.Tx) error {
		store := devsimstore.FromTransaction(tx)
		if err := store.UnknownDispatched(ctx); err != nil {
			return fmt.Errorf("mark operations unknown after transport event gap: %w", err)
		}
		at := s.now().UTC()
		for _, node := range disconnected {
			event, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if err := store.DisconnectNode(ctx, node, event, at); err != nil {
				return fmt.Errorf("record simulator disconnect after transport event gap: %w", err)
			}
		}
		return nil
	}, nil
}

func newIDs() (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ids := make([]uuid.UUID, 5)
	for index := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("generate UUIDv7: %w", err)
		}
		ids[index] = id
	}
	return ids[0], ids[1], ids[2], ids[3], ids[4], nil
}

func normalizeScenario(scenario SimulationScenario) (uint32, uint32, error) {
	heartbeatCount := uint32(3)
	if scenario.HeartbeatCount != nil {
		heartbeatCount = *scenario.HeartbeatCount
	}
	delayMillis := uint32(100)
	if scenario.DelayMillis != nil {
		delayMillis = *scenario.DelayMillis
	}
	if heartbeatCount < 1 || heartbeatCount > 32 || delayMillis > 30_000 {
		return 0, 0, fmt.Errorf("%w: simulation limits exceeded", ErrInvalidScenario)
	}
	return heartbeatCount, delayMillis, nil
}

func marshalEnvelope(nodeID, operationID, commandID uuid.UUID, traceparent string, scenario SimulationScenario, heartbeatCount, delayMillis uint32, now time.Time) ([]byte, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate message ID: %w", err)
	}
	envelope := &agentv1.CommandEnvelope{
		ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: nodeID[:],
		Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), Traceparent: traceparent,
		ActorId: "developer", Reason: "development simulation probe", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
		Payload: &agentv1.CommandEnvelope_SimulationProbe{SimulationProbe: &agentv1.SimulationProbe{
			HeartbeatCount: heartbeatCount, DelayMillis: delayMillis, DuplicateEvent: scenario.DuplicateEvent,
			ReturnError: scenario.ReturnError, DisconnectAfter: scenario.DisconnectAfter,
		}},
	}
	data, err := proto.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal simulator command: %w", err)
	}
	return data, nil
}
