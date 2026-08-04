package localslice

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const workspaceSlug = "local-simulator"

var ErrInvalidScenario = errors.New("invalid simulation")

type Scenario struct {
	HeartbeatCount  *uint32 `json:"heartbeat_count"`
	DelayMillis     *uint32 `json:"delay_millis"`
	DuplicateEvent  bool    `json:"duplicate_event"`
	ReturnError     bool    `json:"return_error"`
	DisconnectAfter bool    `json:"disconnect_after"`
}

type Operation struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	NodeID    *string   `json:"node_id,omitempty"`
	CommandID *string   `json:"command_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Event struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	Type        string    `json:"type"`
	Traceparent string    `json:"traceparent"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type Job struct {
	OperationID uuid.UUID
	NodeID      uuid.UUID
	Envelope    []byte
	Traceparent string
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, scenario Scenario, requestID, traceparent string) (Operation, error) {
	ctx, span := otel.Tracer("ocservia.localslice").Start(ctx, "local_slice.queue", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	heartbeatCount, delayMillis, err := normalizeScenario(scenario)
	if err != nil {
		return Operation{}, err
	}
	if requestID == "" || !validTraceparent(traceparent) {
		return Operation{}, fmt.Errorf("%w: request correlation is required", ErrInvalidScenario)
	}

	now := s.now()
	workspaceID, nodeID, operationID, commandID, auditID, err := newIDs()
	if err != nil {
		return Operation{}, err
	}
	envelope, err := marshalEnvelope(nodeID, operationID, commandID, traceparent, scenario, heartbeatCount, delayMillis, now)
	if err != nil {
		return Operation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, fmt.Errorf("begin local slice transaction: %w", err)
	}
	defer rollback(tx)
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspaces (id, name, slug, created_at, updated_at)
		VALUES ($1, 'Local simulator', $2, $3, $3)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, workspaceID, workspaceSlug, now).Scan(&workspaceID); err != nil {
		return Operation{}, fmt.Errorf("ensure simulator workspace: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO nodes (id, workspace_id, name, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'active', $4, $4)`, nodeID, workspaceID, "sim-"+nodeID.String(), now); err != nil {
		return Operation{}, fmt.Errorf("insert simulator node: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations (id, workspace_id, node_id, command_id, state, request_id, trace_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'queued', $5, $6, $7, $7)`, operationID, workspaceID, nodeID, commandID, requestID, traceID(traceparent), now); err != nil {
		return Operation{}, fmt.Errorf("insert simulator operation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO local_slice_jobs (operation_id, command_envelope, traceparent, available_at, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $4)`, operationID, envelope, traceparent, now, now.Add(time.Minute)); err != nil {
		return Operation{}, fmt.Errorf("insert simulator job: %w", err)
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "development_stub", ActorID: "developer", Action: "simulation.probe", ResourceType: "operation", ResourceID: operationID, RequestID: requestID, TraceID: traceID(traceparent), Reason: "I03 local side-effect-free slice", At: now}); err != nil {
		return Operation{}, fmt.Errorf("append simulator audit intent: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, fmt.Errorf("commit local slice transaction: %w", err)
	}
	nodeIDText, commandIDText := nodeID.String(), commandID.String()
	return Operation{ID: operationID.String(), State: "queued", NodeID: &nodeIDText, CommandID: &commandIDText, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Service) GetOperation(ctx context.Context, id uuid.UUID) (Operation, error) {
	var operation Operation
	var nodeID, commandID pgtype.Text
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, state, node_id::text, command_id::text, created_at, updated_at
		FROM operations WHERE id = $1`, id).Scan(&operation.ID, &operation.State, &nodeID, &commandID, &operation.CreatedAt, &operation.UpdatedAt)
	if err != nil {
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}
	operation.NodeID = optionalText(nodeID)
	operation.CommandID = optionalText(commandID)
	return operation, nil
}

func (s *Service) ListOperations(ctx context.Context, after uuid.UUID, limit int) ([]Operation, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("operation page size must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, state, node_id::text, command_id::text, created_at, updated_at
		FROM operations
		WHERE ($1::uuid IS NULL OR id < $1)
		ORDER BY id DESC LIMIT $2`, nullableUUID(after), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()
	operations := make([]Operation, 0, limit+1)
	for rows.Next() {
		var operation Operation
		var nodeID, commandID pgtype.Text
		if err := rows.Scan(&operation.ID, &operation.State, &nodeID, &commandID, &operation.CreatedAt, &operation.UpdatedAt); err != nil {
			return nil, false, fmt.Errorf("scan operation: %w", err)
		}
		operation.NodeID = optionalText(nodeID)
		operation.CommandID = optionalText(commandID)
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(operations) > limit
	if hasMore {
		operations = operations[:limit]
	}
	return operations, hasMore, nil
}

func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func (s *Service) ListEvents(ctx context.Context, after uuid.UUID, limit int) ([]Event, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("event page size must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id::text, node_id::text, event_type, traceparent, occurred_at
		FROM transport_events
		WHERE ($1::uuid IS NULL OR ingest_sequence > (
			SELECT ingest_sequence FROM transport_events WHERE event_id = $1
		))
		ORDER BY ingest_sequence
		LIMIT $2`, nullableUUID(after), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list transport events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0, limit+1)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.NodeID, &event.Type, &event.Traceparent, &event.OccurredAt); err != nil {
			return nil, false, fmt.Errorf("scan transport event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate transport events: %w", err)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	return events, hasMore, nil
}

func (s *Service) LastEventID(ctx context.Context) ([]byte, error) {
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, "SELECT event_id FROM transport_events WHERE transport_cursor_valid ORDER BY ingest_sequence DESC LIMIT 1").Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read event cursor: %w", err)
	}
	return id[:], nil
}

func (s *Service) ReconcileEventGap(ctx context.Context, nodeConnected func(context.Context, []byte) (bool, error)) error {
	rows, err := s.pool.Query(ctx, `
		SELECT node.id, latest.traceparent
		FROM nodes AS node
		JOIN workspaces AS workspace ON workspace.id = node.workspace_id
		JOIN LATERAL (
			SELECT event.traceparent
			FROM transport_events AS event
			WHERE event.node_id = node.id
			ORDER BY event.ingest_sequence DESC
			LIMIT 1
		) AS latest ON true
		WHERE workspace.slug = $1 AND node.status = 'active'`, workspaceSlug)
	if err != nil {
		return fmt.Errorf("select simulator nodes after transport event gap: %w", err)
	}
	type disconnectedNode struct {
		id          uuid.UUID
		traceparent string
	}
	candidates := make([]disconnectedNode, 0)
	for rows.Next() {
		var node disconnectedNode
		if err := rows.Scan(&node.id, &node.traceparent); err != nil {
			rows.Close()
			return fmt.Errorf("scan simulator node after transport event gap: %w", err)
		}
		candidates = append(candidates, node)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate simulator nodes after transport event gap: %w", err)
	}
	rows.Close()
	disconnected := make([]disconnectedNode, 0, len(candidates))
	for _, node := range candidates {
		connected, err := nodeConnected(ctx, node.id[:])
		if err != nil {
			return fmt.Errorf("reconcile simulator node connection %s: %w", node.id, err)
		}
		if !connected {
			disconnected = append(disconnected, node)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transport event gap reconciliation: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "UPDATE transport_events SET transport_cursor_valid = false WHERE transport_cursor_valid"); err != nil {
		return fmt.Errorf("invalidate retained transport cursor after event gap: %w", err)
	}
	commandTag, err := tx.Exec(ctx, `
		UPDATE operations AS operation
		SET state = 'unknown', updated_at = now()
		FROM local_slice_jobs AS job
		WHERE job.operation_id = operation.id
		  AND job.dispatched_at IS NOT NULL
		  AND operation.state IN ('queued', 'dispatched', 'accepted', 'running')`)
	if err != nil {
		return fmt.Errorf("mark operations unknown after transport event gap: %w", err)
	}
	if commandTag.RowsAffected() > 0 {
		_, err = tx.Exec(ctx, `
			UPDATE local_slice_jobs
			SET last_error = 'transport event retention gap; outcome requires reconciliation'
			WHERE dispatched_at IS NOT NULL
			  AND operation_id IN (SELECT id FROM operations WHERE state = 'unknown')`)
		if err != nil {
			return fmt.Errorf("record transport event gap: %w", err)
		}
	}
	now := s.now()
	for _, node := range disconnected {
		eventID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate transport gap event ID: %w", err)
		}
		result, err := tx.Exec(ctx, `
			UPDATE nodes
			SET status = 'offline', updated_at = $2, version = version + 1
			WHERE id = $1 AND status = 'active'`, node.id, now)
		if err != nil {
			return fmt.Errorf("mark simulator node offline after transport event gap: %w", err)
		}
		if result.RowsAffected() == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO transport_events (event_id, node_id, event_type, occurred_at, traceparent, payload, transport_cursor_valid)
			VALUES ($1, $2, 'disconnected', $3, $4, $5, false)`,
			eventID, node.id, now, node.traceparent, []byte("transport event retention gap")); err != nil {
			return fmt.Errorf("record simulator disconnect after transport event gap: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transport event gap reconciliation: %w", err)
	}
	return nil
}

func (s *Service) Ingest(ctx context.Context, event *transportv1.TransportEvent) error {
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": event.GetTraceparent()})
	ctx, span := otel.Tracer("ocservia.localslice").Start(ctx, "local_slice.event.ingest", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	eventID, err := uuid.FromBytes(event.GetEventId())
	if err != nil || eventID.Version() != 7 {
		return errors.New("event_id must be UUIDv7")
	}
	nodeID, err := uuid.FromBytes(event.GetNodeId())
	if err != nil || nodeID.Version() != 7 {
		return errors.New("node_id must be UUIDv7")
	}
	if !validTraceparent(event.GetTraceparent()) {
		return errors.New("event traceparent is invalid")
	}
	if len(event.GetPayload()) > 1<<20 {
		return errors.New("event payload exceeds 1 MiB")
	}
	eventType, err := eventName(event.GetType())
	if err != nil {
		return err
	}
	if eventType == "telemetry" {
		if _, err := telemetrystore.New(s.pool).IngestWire(ctx, event.GetPayload()); err != nil {
			return fmt.Errorf("ingest telemetry payload: %w", err)
		}
	}
	occurredAt := event.GetOccurredAt()
	if occurredAt == nil || occurredAt.CheckValid() != nil {
		return errors.New("event occurred_at is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event transaction: %w", err)
	}
	defer rollback(tx)
	result, err := tx.Exec(ctx, `
		INSERT INTO transport_events (event_id, node_id, event_type, occurred_at, traceparent, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id) DO NOTHING`, eventID, nodeID, eventType, occurredAt.AsTime(), event.GetTraceparent(), event.GetPayload())
	if err != nil {
		return fmt.Errorf("insert transport event: %w", err)
	}
	if result.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	status := "active"
	if eventType == "disconnected" {
		status = "offline"
	}
	if eventType != "telemetry" {
		if _, err := tx.Exec(ctx, "UPDATE nodes SET status = $2, updated_at = $3, version = version + 1 WHERE id = $1 AND status IN ('active','offline')", nodeID, status, occurredAt.AsTime()); err != nil {
			return fmt.Errorf("update node from transport event: %w", err)
		}
	}
	operationState := "running"
	switch eventType {
	case "command_result":
		operationState = "succeeded"
	case "error":
		operationState = "failed"
	case "disconnected":
		operationState = "unknown"
	}
	if eventType == "disconnected" {
		if _, err := tx.Exec(ctx, `
			UPDATE operations AS operation
			SET state = 'unknown', updated_at = $2, version = version + 1
			FROM local_slice_jobs AS job
			WHERE operation.id = job.operation_id AND operation.node_id = $1
			  AND job.dispatched_at IS NOT NULL
			  AND operation.state NOT IN ('succeeded', 'failed', 'expired', 'rolled_back', 'superseded')`,
			nodeID, occurredAt.AsTime()); err != nil {
			return fmt.Errorf("mark disconnected operations unknown: %w", err)
		}
	} else if eventType != "telemetry" && eventType != "path_changed" {
		if _, err := tx.Exec(ctx, `
			UPDATE operations AS operation
			SET state = $2, updated_at = $3, version = version + 1
			FROM local_slice_jobs AS job
			WHERE operation.id = job.operation_id AND operation.node_id = $1
			  AND job.traceparent = $4
			  AND operation.state NOT IN ('succeeded', 'failed', 'expired', 'rolled_back', 'superseded')`,
			nodeID, operationState, occurredAt.AsTime(), event.GetTraceparent()); err != nil {
			return fmt.Errorf("update operation from transport event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event transaction: %w", err)
	}
	return nil
}

func (s *Service) ClaimJobs(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
			SELECT job.operation_id
			FROM local_slice_jobs AS job
			JOIN operations AS operation ON operation.id = job.operation_id
			WHERE job.available_at <= now()
			  AND job.expires_at > now()
			  AND job.dispatched_at IS NULL
			  AND operation.state = 'queued'
			ORDER BY job.available_at, job.operation_id
			FOR UPDATE OF job SKIP LOCKED
			LIMIT $1
		)
		UPDATE local_slice_jobs AS job
		SET available_at = now() + interval '10 seconds'
		FROM candidates
		WHERE job.operation_id = candidates.operation_id
		RETURNING job.operation_id,
		  (SELECT node_id FROM operations WHERE id = job.operation_id),
		  job.command_envelope,
		  job.traceparent`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim simulator jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]Job, 0, limit)
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.OperationID, &job.NodeID, &job.Envelope, &job.Traceparent); err != nil {
			return nil, fmt.Errorf("scan simulator job: %w", err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Service) ExpireJobs(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		WITH expired AS (
			UPDATE operations AS operation
			SET state = 'expired', updated_at = now(), version = version + 1
			FROM local_slice_jobs AS job
			WHERE operation.id = job.operation_id
			  AND job.dispatched_at IS NULL
			  AND job.expires_at <= now()
			  AND operation.state NOT IN ('succeeded', 'failed', 'expired', 'rolled_back', 'superseded')
			RETURNING operation.id
		)
		UPDATE local_slice_jobs AS job
		SET last_error = 'command expired before dispatch'
		FROM expired
		WHERE job.operation_id = expired.id`)
	if err != nil {
		return fmt.Errorf("expire simulator jobs: %w", err)
	}
	return nil
}

func (s *Service) MarkDispatchStarted(ctx context.Context, operationID uuid.UUID) error {
	result, err := s.pool.Exec(ctx, `
		WITH started AS (
			UPDATE operations
			SET state = 'dispatched', updated_at = now(), version = version + 1
			WHERE id = $1 AND state = 'queued'
			RETURNING id
		)
		UPDATE local_slice_jobs AS job
		SET dispatched_at = now(), attempts = attempts + 1, last_error = NULL
		FROM started
		WHERE job.operation_id = started.id AND job.dispatched_at IS NULL`, operationID)
	if err != nil {
		return fmt.Errorf("mark simulator dispatch started: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("simulator job is no longer dispatchable")
	}
	return nil
}

func (s *Service) MarkDispatchError(ctx context.Context, operationID uuid.UUID, message string) error {
	if len(message) > 512 {
		message = message[:512]
	}
	_, err := s.pool.Exec(ctx, `
		WITH retryable AS (
			UPDATE operations
			SET state = 'queued', updated_at = now(), version = version + 1
			WHERE id = $1 AND state = 'dispatched'
			RETURNING id
		)
		UPDATE local_slice_jobs AS job
		SET dispatched_at = NULL, last_error = $2, available_at = now() + interval '1 second'
		FROM retryable
		WHERE job.operation_id = retryable.id`, operationID, message)
	if err != nil {
		return fmt.Errorf("record simulator dispatch error: %w", err)
	}
	return nil
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

func normalizeScenario(scenario Scenario) (uint32, uint32, error) {
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

func marshalEnvelope(nodeID, operationID, commandID uuid.UUID, traceparent string, scenario Scenario, heartbeatCount, delayMillis uint32, now time.Time) ([]byte, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate message ID: %w", err)
	}
	envelope := &agentv1.CommandEnvelope{
		ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: nodeID[:],
		Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)), Traceparent: traceparent,
		ActorId: "developer", Reason: "I03 local side-effect-free slice",
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

func eventName(value transportv1.TransportEventType) (string, error) {
	switch value {
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_CONNECTED:
		return "connected", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED:
		return "disconnected", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT:
		return "command_result", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_HEARTBEAT:
		return "heartbeat", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_ERROR:
		return "error", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_PATH_CHANGED:
		return "path_changed", nil
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_TELEMETRY:
		return "telemetry", nil
	default:
		return "", errors.New("unsupported transport event type")
	}
}

func validTraceparent(value string) bool {
	parts := [4]string{}
	count := 0
	var start int
	for index := 0; index <= len(value); index++ {
		if index == len(value) || value[index] == '-' {
			if count >= len(parts) {
				return false
			}
			parts[count] = value[start:index]
			count++
			start = index + 1
		}
	}
	if count != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, part := range parts {
		for index := range len(part) {
			character := part[index]
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
		if len(part)%2 != 0 {
			return false
		}
	}
	return parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}

func traceID(traceparent string) string { return traceparent[3:35] }

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
