package localslice

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/postgresinput"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Event struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	Type        string    `json:"type"`
	Traceparent string    `json:"traceparent"`
	OccurredAt  time.Time `json:"occurred_at"`
	Sequence    int64     `json:"-"`
}

type Job struct {
	OperationID uuid.UUID
	NodeID      uuid.UUID
	Envelope    []byte
	Traceparent string
}

type Service struct {
	pool                   *pgxpool.Pool
	now                    func() time.Time
	signer                 *commandauth.Signer
	commandRecovery        CommandRecoverer
	recoveryAuthority      RecoveryAuthority
	resultCommitBarrierDir string
}

// CommandRecoverer is the narrow business-layer hook used after transportd
// reports that a fenced Agent connection is registered and usable. The
// implementation must recheck PostgreSQL ownership inside the supplied event
// transaction before it changes command or outbox state.
type CommandRecoverer interface {
	RecoverAmbiguousDispatchedTx(context.Context, pgx.Tx, operationstore.OwnerReconnect) (int, error)
}

// RecoveryAuthority binds reconnect scheduling to the process that owns the
// exact local session term. PostgreSQL is still rechecked by CommandRecoverer
// inside the state-changing transaction.
type RecoveryAuthority interface {
	OwnsTerm(nodeID, connectionID [16]byte, ownerEpoch int64) bool
}

const (
	maxTransportEventAge = telemetrystore.MaxTelemetryAge
	maxQuarantineDetail  = 256
)

type permanentInvalidEvent struct {
	reasonCode string
	detail     string
}

func (e *permanentInvalidEvent) Error() string { return e.detail }

func invalidEvent(reasonCode, detail string) error {
	if len(detail) > maxQuarantineDetail {
		detail = detail[:maxQuarantineDetail]
	}
	return &permanentInvalidEvent{reasonCode: reasonCode, detail: detail}
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

// NewWithSigner configures recovery redispatches with a Controller signer.
func NewWithSigner(pool *pgxpool.Pool, signer *commandauth.Signer) *Service {
	service := New(pool)
	service.signer = signer
	return service
}

// NewWithCommandRecovery enables authoritative reconnect reconciliation while
// leaving ownership acquisition and renewal inside ownersession.
func NewWithCommandRecovery(pool *pgxpool.Pool, signer *commandauth.Signer, recovery CommandRecoverer, authority RecoveryAuthority) *Service {
	service := NewWithSigner(pool, signer)
	service.commandRecovery, service.recoveryAuthority = recovery, authority
	return service
}

// EnableResultCommitBarrier configures the development-harness barrier used
// to stop a validated command result inside its still-open database
// transaction. Production configuration rejects this hook before startup.
func (s *Service) EnableResultCommitBarrier(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("result commit barrier path is not a directory")
	}
	s.resultCommitBarrierDir = directory
	return nil
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
	simulatorEndpoint := sha256.Sum256(append([]byte("ocservia/development-simulator/"), nodeID[:]...))
	if _, err := tx.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, nodeID, simulatorEndpoint[:], now); err != nil {
		return Operation{}, fmt.Errorf("bind simulator endpoint: %w", err)
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
	return Operation{ID: operationID.String(), State: "queued", NodeID: &nodeIDText, CommandID: &commandIDText, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Service) GetOperation(ctx context.Context, id uuid.UUID) (Operation, error) {
	var operation Operation
	var nodeID, commandID pgtype.Text
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, state, node_id::text, command_id::text, version, created_at, updated_at
		FROM operations WHERE id = $1`, id).Scan(&operation.ID, &operation.State, &nodeID, &commandID, &operation.Version, &operation.CreatedAt, &operation.UpdatedAt)
	if err != nil {
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}
	operation.NodeID = optionalText(nodeID)
	operation.CommandID = optionalText(commandID)
	return operation, nil
}

func (s *Service) ListOperations(ctx context.Context, after uuid.UUID, limit int) ([]Operation, bool, error) {
	return s.ListOperationsInWorkspace(ctx, uuid.Nil, after, limit)
}

func (s *Service) ListOperationsInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Operation, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("operation page size must be between 1 and 200")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, state, node_id::text, command_id::text, version, created_at, updated_at
		FROM operations
		WHERE ($1::uuid IS NULL OR id < $1) AND ($3::uuid IS NULL OR workspace_id=$3)
		ORDER BY id DESC LIMIT $2`, nullableUUID(after), limit+1, nullableUUID(workspaceID))
	if err != nil {
		return nil, false, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()
	operations := make([]Operation, 0, limit+1)
	for rows.Next() {
		var operation Operation
		var nodeID, commandID pgtype.Text
		if err := rows.Scan(&operation.ID, &operation.State, &nodeID, &commandID, &operation.Version, &operation.CreatedAt, &operation.UpdatedAt); err != nil {
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
	return s.ListEventsInWorkspace(ctx, uuid.Nil, after, limit, ListEventsAscending)
}

// Event list orders. The desc order lets clients read the newest events with
// a bounded number of pages instead of walking the whole durable history.
const (
	ListEventsAscending  = "asc"
	ListEventsDescending = "desc"
)

func (s *Service) ListEventsInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int, order string) ([]Event, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("event page size must be between 1 and 200")
	}
	if order != ListEventsAscending && order != ListEventsDescending {
		return nil, false, errors.New("event order must be asc or desc")
	}
	query := `
		SELECT event.event_id::text, event.node_id::text, event.event_type, event.traceparent, event.occurred_at, event.ingest_sequence
		FROM transport_events event JOIN nodes node ON node.id=event.node_id
		WHERE ($1::uuid IS NULL OR event.ingest_sequence > (
			SELECT ingest_sequence FROM transport_events WHERE event_id = $1
		)) AND ($3::uuid IS NULL OR node.workspace_id=$3)
		ORDER BY event.ingest_sequence
		LIMIT $2`
	if order == ListEventsDescending {
		query = `
		SELECT event.event_id::text, event.node_id::text, event.event_type, event.traceparent, event.occurred_at, event.ingest_sequence
		FROM transport_events event JOIN nodes node ON node.id=event.node_id
		WHERE ($1::uuid IS NULL OR event.ingest_sequence < (
			SELECT ingest_sequence FROM transport_events WHERE event_id = $1
		)) AND ($3::uuid IS NULL OR node.workspace_id=$3)
		ORDER BY event.ingest_sequence DESC
		LIMIT $2`
	}
	rows, err := s.pool.Query(ctx, query, nullableUUID(after), limit+1, nullableUUID(workspaceID))
	if err != nil {
		return nil, false, fmt.Errorf("list transport events: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0, limit+1)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.NodeID, &event.Type, &event.Traceparent, &event.OccurredAt, &event.Sequence); err != nil {
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

func (s *Service) EventSequenceInWorkspace(ctx context.Context, workspaceID, eventID uuid.UUID) (int64, bool, error) {
	if workspaceID == uuid.Nil || eventID == uuid.Nil {
		return 0, false, nil
	}
	var sequence int64
	err := s.pool.QueryRow(ctx, `
		SELECT event.ingest_sequence
		FROM transport_events event JOIN nodes node ON node.id=event.node_id
		WHERE event.event_id=$1 AND node.workspace_id=$2`, eventID, workspaceID).Scan(&sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve transport event cursor: %w", err)
	}
	return sequence, true, nil
}

func (s *Service) LastEventID(ctx context.Context) ([]byte, error) {
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, "SELECT event_id FROM transport_event_cursor WHERE singleton AND valid").Scan(&id); errors.Is(err, pgx.ErrNoRows) {
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
	if _, err := tx.Exec(ctx, "UPDATE transport_event_cursor SET valid=false,updated_at=now() WHERE singleton"); err != nil {
		return fmt.Errorf("invalidate durable transport cursor after event gap: %w", err)
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
	observedAt := s.now()
	eventID, err := uuid.FromBytes(event.GetEventId())
	if err != nil || eventID.Version() != 7 {
		return errors.New("transport event_id must be UUIDv7")
	}
	nodeID, err := uuid.FromBytes(event.GetNodeId())
	if err != nil || nodeID.Version() != 7 {
		return errors.New("transport node_id must be UUIDv7")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event transaction: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SAVEPOINT transport_event_business"); err != nil {
		return fmt.Errorf("create transport event savepoint: %w", err)
	}
	workspaceID, inserted, err := s.ingestTransportEventTx(ctx, tx, eventID, nodeID, event, observedAt)
	if err != nil {
		var permanent *permanentInvalidEvent
		if !errors.As(err, &permanent) {
			return err
		}
		if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT transport_event_business"); rollbackErr != nil {
			return fmt.Errorf("rollback permanently invalid transport event: %w", rollbackErr)
		}
		if _, releaseErr := tx.Exec(ctx, "RELEASE SAVEPOINT transport_event_business"); releaseErr != nil {
			return fmt.Errorf("release transport event savepoint: %w", releaseErr)
		}
		if err := quarantineTransportEvent(ctx, tx, eventID, nodeID, event, workspaceID, observedAt, permanent); err != nil {
			return err
		}
	} else {
		if inserted && event.GetType() == transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_CONNECTED && s.commandRecovery != nil && s.recoveryAuthority != nil {
			connectionID, fenced, termErr := connectedOwnerTerm(event)
			if termErr != nil {
				return termErr
			}
			if fenced && s.recoveryAuthority.OwnsTerm([16]byte(nodeID), connectionID, int64(event.GetOwnerEpoch())) {
				recovered, recoveryErr := s.commandRecovery.RecoverAmbiguousDispatchedTx(ctx, tx, operationstore.OwnerReconnect{
					NodeID: nodeID, ConnectionID: connectionID, OwnerEpoch: event.GetOwnerEpoch(),
					ObservedAt: observedAt, Limit: operationstore.DefaultReconnectRecoveryLimit,
				})
				if recoveryErr != nil && !errors.Is(recoveryErr, connectionowner.ErrNotOwner) {
					return fmt.Errorf("recover commands after authoritative reconnect: %w", recoveryErr)
				}
				span.SetAttributes(attribute.Int("command.recovery.scheduled", recovered))
			}
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT transport_event_business"); err != nil {
			return fmt.Errorf("release transport event savepoint: %w", err)
		}
		if err := advanceTransportCursor(ctx, tx, eventID, observedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event transaction: %w", err)
	}
	return nil
}

func (s *Service) waitAtResultCommitBarrier(ctx context.Context, event *transportv1.TransportEvent, observedAt time.Time) error {
	if s.resultCommitBarrierDir == "" || event.GetType() != transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT {
		return nil
	}
	armed, err := os.ReadFile(filepath.Join(s.resultCommitBarrierDir, "arm"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read result commit barrier: %w", err)
	}
	var result agentv1.CommandResult
	if err := proto.Unmarshal(event.GetPayload(), &result); err != nil {
		return nil
	}
	commandID, err := uuid.FromBytes(result.GetCommandId())
	if err != nil || string(bytes.TrimSpace(armed)) != commandID.String() {
		return nil
	}
	received := []byte(commandID.String() + "\n" + observedAt.UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(filepath.Join(s.resultCommitBarrierDir, "received"), received, 0o600); err != nil {
		return fmt.Errorf("signal result commit barrier: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			release, readErr := os.ReadFile(filepath.Join(s.resultCommitBarrierDir, "release"))
			if readErr == nil && string(bytes.TrimSpace(release)) == commandID.String() {
				return nil
			}
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return fmt.Errorf("read result commit barrier release: %w", readErr)
			}
		}
	}
}

func (s *Service) ingestTransportEventTx(ctx context.Context, tx pgx.Tx, eventID, nodeID uuid.UUID, event *transportv1.TransportEvent, observedAt time.Time) (uuid.UUID, bool, error) {
	var workspaceID uuid.UUID
	var nodeStatus, endpointState string
	if err := tx.QueryRow(ctx, `SELECT n.workspace_id,n.status,k.state
		FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id
		WHERE n.id=$1 AND k.endpoint_id=$2
		FOR SHARE OF n,k`, nodeID, event.GetEndpointId()).Scan(&workspaceID, &nodeStatus, &endpointState); errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, invalidEvent("node_endpoint_not_active", "transport event node and EndpointID are not authoritatively active")
	} else if err != nil {
		return uuid.Nil, false, fmt.Errorf("check authoritative node ingress trust: %w", err)
	}
	eventType, err := eventName(event.GetType())
	if err != nil {
		return workspaceID, false, invalidEvent("unsupported_event_type", err.Error())
	}
	if eventType == "connected" {
		if _, _, err := connectedOwnerTerm(event); err != nil {
			return workspaceID, false, invalidEvent("invalid_owner_term", err.Error())
		}
	}
	activeIngress := (nodeStatus == "active" || nodeStatus == "offline") && endpointState == "active"
	// Revocation commits the tombstone before transportd closes the old session.
	// Only its exact node/endpoint terminal event may cross that closed trust boundary.
	revokedTerminalDisconnect := eventType == "disconnected" && nodeStatus == "revoked" && endpointState == "revoked"
	if !activeIngress && !revokedTerminalDisconnect {
		return workspaceID, false, invalidEvent("node_endpoint_not_active", "transport event node is not authoritatively active")
	}
	if len(event.GetEndpointId()) != 32 {
		return workspaceID, false, invalidEvent("invalid_endpoint_id", "transport event endpoint_id must be 32 bytes")
	}
	if !validTraceparent(event.GetTraceparent()) {
		return workspaceID, false, invalidEvent("invalid_traceparent", "transport event traceparent is invalid")
	}
	if len(event.GetPayload()) > 1<<20 {
		return workspaceID, false, invalidEvent("payload_too_large", "transport event payload exceeds 1 MiB")
	}
	occurredAt := event.GetOccurredAt()
	if occurredAt == nil || occurredAt.CheckValid() != nil {
		return workspaceID, false, invalidEvent("invalid_occurred_at", "transport event occurred_at is invalid")
	}
	occurredTime := occurredAt.AsTime()
	if occurredTime.Before(observedAt.Add(-maxTransportEventAge)) || occurredTime.After(observedAt.Add(telemetrystore.MaxTelemetrySkew)) {
		return workspaceID, false, invalidEvent("occurred_at_out_of_range", "transport event occurred_at is outside the accepted retention window")
	}
	result, err := tx.Exec(ctx, `
		INSERT INTO transport_events (event_id, node_id, event_type, occurred_at, traceparent, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id) DO NOTHING`, eventID, nodeID, eventType, occurredTime, event.GetTraceparent(), event.GetPayload())
	if err != nil {
		return workspaceID, false, fmt.Errorf("insert transport event: %w", err)
	}
	if result.RowsAffected() == 0 {
		return workspaceID, false, nil
	}
	if eventType == "telemetry" {
		if _, err := telemetrystore.New(s.pool).IngestWireTx(ctx, tx, nodeID, event.GetPayload()); err != nil {
			if errors.Is(err, telemetrystore.ErrInvalidTelemetry) {
				return workspaceID, false, invalidEvent("invalid_telemetry", err.Error())
			}
			return workspaceID, false, fmt.Errorf("ingest telemetry payload: %w", err)
		}
	}
	structuredCommandResult := eventType == "command_result"
	if structuredCommandResult {
		if err := ingestAgentCommandResult(ctx, tx, eventID, nodeID, event.GetPayload(), occurredTime, observedAt, s.signer); err != nil {
			return workspaceID, false, err
		}
		if err := s.waitAtResultCommitBarrier(ctx, event, observedAt); err != nil {
			return workspaceID, false, err
		}
	}
	status := "active"
	if eventType == "disconnected" {
		status = "offline"
	}
	if eventType != "telemetry" {
		// Routine same-state signals must not invalidate node mutation preconditions.
		if _, err := tx.Exec(ctx, "UPDATE nodes SET status = $2, updated_at = $3, version = version + 1 WHERE id = $1 AND status IN ('active','offline') AND status IS DISTINCT FROM $2", nodeID, status, occurredTime); err != nil {
			return workspaceID, false, fmt.Errorf("update node from transport event: %w", err)
		}
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
	if eventType == "disconnected" {
		if _, err := tx.Exec(ctx, `
			UPDATE operations AS operation
			SET state = 'unknown', updated_at = $2, version = version + 1
			FROM local_slice_jobs AS job
			WHERE operation.id = job.operation_id AND operation.node_id = $1
			  AND job.dispatched_at IS NOT NULL
			  AND operation.state NOT IN ('succeeded', 'failed', 'expired', 'rolled_back', 'superseded')`,
			nodeID, occurredTime); err != nil {
			return workspaceID, false, fmt.Errorf("mark disconnected operations unknown: %w", err)
		}
	} else if eventType != "telemetry" && eventType != "path_changed" && !structuredCommandResult {
		if _, err := tx.Exec(ctx, `
			UPDATE operations AS operation
			SET state = $2, updated_at = $3, version = version + 1
			FROM local_slice_jobs AS job
			WHERE operation.id = job.operation_id AND operation.node_id = $1
			  AND job.traceparent = $4
			  AND operation.state NOT IN ('succeeded', 'failed', 'expired', 'rolled_back', 'superseded')`,
			nodeID, operationState, occurredTime, event.GetTraceparent()); err != nil {
			return workspaceID, false, fmt.Errorf("update operation from transport event: %w", err)
		}
	}
	return workspaceID, true, nil
}

func quarantineTransportEvent(ctx context.Context, tx pgx.Tx, eventID, nodeID uuid.UUID, event *transportv1.TransportEvent, workspaceID uuid.UUID, observedAt time.Time, failure *permanentInvalidEvent) error {
	payloadHash := sha256.Sum256(event.GetPayload())
	result, err := tx.Exec(ctx, `INSERT INTO transport_event_quarantine
		(event_id,node_id,event_type,payload_sha256,reason_code,reason_detail,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO NOTHING`,
		eventID, nodeID, int32(event.GetType()), payloadHash[:], failure.reasonCode, failure.detail, observedAt)
	if err != nil {
		return fmt.Errorf("quarantine permanently invalid transport event: %w", err)
	}
	if result.RowsAffected() == 0 {
		var existingNode uuid.UUID
		var existingType int32
		var existingHash []byte
		if err := tx.QueryRow(ctx, `SELECT node_id,event_type,payload_sha256 FROM transport_event_quarantine WHERE event_id=$1`, eventID).Scan(&existingNode, &existingType, &existingHash); err != nil {
			return fmt.Errorf("read quarantined transport event: %w", err)
		}
		if existingNode != nodeID || existingType != int32(event.GetType()) || !bytes.Equal(existingHash, payloadHash[:]) {
			return errors.New("transport event ID collides with different quarantine evidence")
		}
	} else if workspaceID != uuid.Nil {
		alertID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate transport quarantine alert ID: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO security_alerts
			(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at)
			VALUES($1,$2,'high','transport_event.permanent_invalid',$3,'transport_event',$4,$5)`,
			alertID, workspaceID, nodeID, eventID, observedAt); err != nil {
			return fmt.Errorf("emit transport quarantine security alert: %w", err)
		}
	}
	return advanceTransportCursor(ctx, tx, eventID, observedAt)
}

func advanceTransportCursor(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, observedAt time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO transport_event_cursor(singleton,event_id,valid,updated_at)
		VALUES(true,$1,true,$2)
		ON CONFLICT(singleton) DO UPDATE SET event_id=EXCLUDED.event_id,valid=true,updated_at=EXCLUDED.updated_at`, eventID, observedAt); err != nil {
		return fmt.Errorf("advance durable transport cursor: %w", err)
	}
	return nil
}

func ingestAgentCommandResult(ctx context.Context, tx pgx.Tx, eventID, nodeID uuid.UUID, payload []byte, occurredAt, observedAt time.Time, signer *commandauth.Signer) error {
	var result agentv1.CommandResult
	if err := proto.Unmarshal(payload, &result); err != nil {
		return invalidCommandResult("structured Agent command result protobuf is invalid")
	}
	commandID, err := uuid.FromBytes(result.GetCommandId())
	if err != nil || commandID.Version() != 7 {
		return invalidCommandResult("command result command_id must be UUIDv7")
	}
	idempotencyKey, err := uuid.FromBytes(result.GetIdempotencyKey())
	if err != nil || idempotencyKey.Version() != 7 {
		return invalidCommandResult("command result idempotency_key must be UUIDv7")
	}
	completedAt := result.GetCompletedAt()
	if completedAt == nil || completedAt.CheckValid() != nil {
		return invalidCommandResult("command result completed_at is invalid")
	}
	completedTime := completedAt.AsTime()
	if completedTime.After(observedAt.Add(5*time.Minute)) || completedTime.After(occurredAt.Add(5*time.Minute)) {
		return invalidCommandResult("command result completed_at exceeds clock skew bound")
	}
	errorCodeValue := result.GetErrorCode()
	if len(result.GetResult()) > 1<<20 || (errorCodeValue != "" && !postgresinput.ValidText(errorCodeValue, 128)) {
		return invalidCommandResult("command result field is invalid")
	}
	state := ""
	switch result.GetState() {
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED:
		state = "succeeded"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_FAILED:
		state = "failed"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_UNKNOWN:
		state = "unknown"
	case agentv1.CommandResultState_COMMAND_RESULT_STATE_REJECTED:
		state = "rejected"
	default:
		return invalidCommandResult("command result state is invalid")
	}
	var envelopeBytes []byte
	var currentState string
	var commandCreatedAt time.Time
	var dispatchInFlight bool
	var inFlightAttemptID, inFlightLeaseToken uuid.UUID
	if err := tx.QueryRow(ctx, `WITH locked_outbox AS MATERIALIZED (
		SELECT id,locked_by,locked_until,attempts
		FROM outbox_events
		WHERE command_id=$1
		FOR UPDATE
	)
		SELECT command.envelope,command.state,command.created_at,
		in_flight.attempt_id IS NOT NULL,
		COALESCE(in_flight.attempt_id,'00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(in_flight.lease_token,'00000000-0000-0000-0000-000000000000'::uuid)
		FROM commands AS command
		JOIN operations AS operation ON operation.command_id=command.id
		LEFT JOIN LATERAL (
			SELECT attempt.id AS attempt_id,lease.lease_token
			FROM locked_outbox AS outbox
			JOIN node_command_leases AS lease ON lease.command_id=command.id
			JOIN command_attempts AS attempt
			  ON attempt.command_id=lease.command_id
			 AND attempt.outbox_event_id=outbox.id
			 AND attempt.worker_id=lease.worker_id
			WHERE lease.command_id=command.id
			  AND lease.node_id=command.node_id
			  AND lease.leased_until>clock_timestamp()
			  AND outbox.locked_by=lease.worker_id
			  AND outbox.locked_until>clock_timestamp()
			  AND attempt.attempt_number=outbox.attempts
			  AND attempt.state='sending'
			  AND attempt.finished_at IS NULL
			LIMIT 1
		) AS in_flight ON true
		WHERE command.id=$1 AND command.node_id=$2`, commandID, nodeID).Scan(
		&envelopeBytes, &currentState, &commandCreatedAt, &dispatchInFlight, &inFlightAttemptID, &inFlightLeaseToken,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return invalidCommandResult("command result does not match a dispatched command")
		}
		return fmt.Errorf("load command envelope for result: %w", err)
	}
	if currentState == "queued" && !dispatchInFlight {
		return invalidCommandResult("queued command result has no current sending attempt")
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		return invalidCommandResult("stored command envelope is invalid")
	}
	if !bytes.Equal(envelope.GetIdempotencyKey(), result.GetIdempotencyKey()) {
		return invalidCommandResult("command result idempotency key mismatch")
	}
	storedHashVersion := envelope.GetSemanticPayloadHashVersion()
	if err := semanticpayload.ValidateVersion(storedHashVersion); err != nil {
		return invalidCommandResult("stored command semantic payload hash version is invalid")
	}
	resultHashVersion := result.GetSemanticPayloadHashVersion()
	if err := semanticpayload.ValidateVersion(resultHashVersion); err != nil {
		return invalidCommandResult("command result semantic payload hash version is invalid")
	}
	issuedAt := envelope.GetIssuedAt()
	if issuedAt == nil || issuedAt.CheckValid() != nil {
		return invalidCommandResult("stored command issued_at is invalid")
	}
	issuedTime := issuedAt.AsTime()
	lowerBound := issuedTime
	if commandCreatedAt.After(lowerBound) {
		lowerBound = commandCreatedAt
	}
	if completedTime.Before(lowerBound.Add(-5 * time.Minute)) {
		return invalidCommandResult("command result completed_at precedes issued_at")
	}
	acceptedAt := result.GetAcceptedAt()
	if state == "rejected" {
		if acceptedAt != nil || result.GetErrorCode() == "" || len(result.GetResult()) != 0 || (len(result.GetPayloadSha256()) != 0 && len(result.GetPayloadSha256()) != sha256.Size) {
			return invalidCommandResult("rejected command result fields are invalid")
		}
	} else {
		if acceptedAt == nil || acceptedAt.CheckValid() != nil || len(result.GetPayloadSha256()) != sha256.Size {
			return invalidCommandResult("accepted command result fields are invalid")
		}
		if acceptedAt.AsTime().Before(lowerBound.Add(-5*time.Minute)) || acceptedAt.AsTime().After(completedTime) {
			return invalidCommandResult("command result accepted_at is invalid")
		}
		if state == "succeeded" && result.GetErrorCode() != "" {
			return invalidCommandResult("succeeded command result must not contain an error code")
		}
		if (state == "failed" || state == "unknown") && result.GetErrorCode() == "" {
			return invalidCommandResult("non-success command result requires an error code")
		}
		if state == "unknown" && len(result.GetResult()) != 0 {
			return invalidCommandResult("unknown command result must not contain result bytes")
		}
	}
	if state == "rejected" && len(result.GetPayloadSha256()) == 0 {
		if resultHashVersion != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED {
			return invalidCommandResult("rejected command result must not declare a payload hash version")
		}
	} else if resultHashVersion != storedHashVersion {
		return invalidCommandResult("command result semantic payload hash version mismatch")
	}
	if len(result.GetPayloadSha256()) == sha256.Size {
		var expectedHash [sha256.Size]byte
		var err error
		switch resultHashVersion {
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1:
			expectedHash, err = semanticpayload.HashV1(&envelope)
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2:
			expectedHash, err = semanticpayload.HashV2(&envelope)
		case agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_UNSPECIFIED:
			expectedHash, err = agentPayloadHash(&envelope)
		}
		if err != nil {
			return invalidCommandResult("stored command semantic payload cannot be hashed")
		}
		if !bytes.Equal(result.GetPayloadSha256(), expectedHash[:]) {
			return invalidCommandResult("command result payload hash mismatch")
		}
	}
	var payloadHash any
	if len(result.GetPayloadSha256()) == 32 {
		payloadHash = result.GetPayloadSha256()
	}
	hashVersion := int16(result.GetSemanticPayloadHashVersion())
	var acceptedAtValue any
	if acceptedAt != nil {
		acceptedAtValue = acceptedAt.AsTime()
	}
	errorCode := any(nil)
	if result.GetErrorCode() != "" {
		errorCode = result.GetErrorCode()
	}
	resultBytes := result.GetResult()
	if resultBytes == nil {
		resultBytes = []byte{}
	}
	verification := privdattestation.VerifyResult(ctx, tx, nodeID, &envelope, &result)
	normalizationState := state
	if verification.Status != "not_required" && !verification.Verified() {
		normalizationState = "unknown"
	}
	effectiveState, applyResult, normalizationErr := normalizeConfigApplyResult(&envelope, normalizationState, resultBytes)
	csrResult, csrErr := normalizeCertificateCSRResult(&envelope, normalizationState, resultBytes)
	revokeResult, revokeErr := normalizeCertificateRevokeResult(&envelope, normalizationState, resultBytes)
	artifactResult, artifactErr := normalizeCertificateArtifactResult(&envelope, normalizationState, resultBytes)
	if normalizationErr == nil && csrErr != nil {
		normalizationErr = csrErr
	}
	if normalizationErr == nil && revokeErr != nil {
		normalizationErr = revokeErr
	}
	if normalizationErr == nil && artifactErr != nil {
		normalizationErr = artifactErr
	}
	recoveryReason := result.GetErrorCode()
	if verification.Status != "not_required" && !verification.Verified() {
		normalizationErr = errors.New("privileged result receipt verification failed")
		recoveryReason = verification.FailureReason
	}
	if normalizationErr != nil {
		effectiveState = "unknown"
		applyResult = nil
		recoveryReason = "outcome_requires_reconciliation"
	}
	terminalEvidenceOnly := false
	if (currentState == "succeeded" || currentState == "failed" || currentState == "rejected" || currentState == "rolled_back") && currentState != effectiveState {
		if normalizationErr != nil {
			// A forged duplicate cannot change an already terminal outcome, but its
			// verification evidence still has to reach the durable alert and audit
			// path below.
			terminalEvidenceOnly = true
		} else {
			return invalidCommandResult("command result contradicts a terminal state")
		}
	}
	if currentState == "expired" || currentState == "superseded" {
		return invalidCommandResult("command result contradicts a terminal state")
	}
	if verification.Verified() {
		var existingCommandID uuid.UUID
		var existingReceipt []byte
		err := tx.QueryRow(ctx, `SELECT command_id,receipt_sha256 FROM agent_command_results WHERE privd_attestation_key_id=$1 AND effect_record_id=$2 AND effect_sequence=$3 AND receipt_verification_status='verified'`, verification.KeyID, verification.EffectRecordID, verification.EffectSequence).Scan(&existingCommandID, &existingReceipt)
		if err == nil {
			if existingCommandID == commandID && bytes.Equal(existingReceipt, verification.ReceiptSHA256) {
				return nil
			}
			return invalidCommandResult("privd effect receipt was replayed across command identities")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check privd effect receipt replay: %w", err)
		}
	}
	var failureReason any
	if verification.FailureReason != "" {
		failureReason = verification.FailureReason
	}
	var keyID any
	if verification.KeyID != "" {
		keyID = verification.KeyID
	}
	// Timestamp results and attempts in PostgreSQL so MarkSent can compare
	// them without process clock skew.
	if _, err := tx.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,error_code,accepted_at,completed_at,replayed,created_at,receipt_verification_status,receipt_failure_reason,privd_attestation_key_id,effect_record_id,effect_sequence,receipt_sha256,privileged_result_proof) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,clock_timestamp(),$12,$13,$14,$15,$16,$17,$18)`, eventID, commandID, idempotencyKey, payloadHash, hashVersion, state, resultBytes, errorCode, acceptedAtValue, completedTime, result.GetReplayed(), verification.Status, failureReason, keyID, nullableBytes(verification.EffectRecordID), nullableUint64(verification.EffectSequence), nullableBytes(verification.ReceiptSHA256), nullableBytes(verification.EncodedProof)); err != nil {
		return fmt.Errorf("persist Agent command result: %w", err)
	}
	if verification.Status != "not_required" && !verification.Verified() {
		var alertWorkspaceID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1`, nodeID).Scan(&alertWorkspaceID); err != nil {
			return fmt.Errorf("load privd receipt alert workspace: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,created_at) VALUES($1,$2,'critical','privd.receipt_verification_failed',$3)`, uuid.Must(uuid.NewV7()), alertWorkspaceID, observedAt); err != nil {
			return fmt.Errorf("emit privd receipt verification alert: %w", err)
		}
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
			WorkspaceID: alertWorkspaceID, ActorType: "controller", ActorID: "privd-receipt-verifier",
			Action: "privd.result.verify", ResourceType: "command", ResourceID: commandID,
			NodeID: &nodeID, CommandID: &commandID, RequestID: eventID.String(), Result: "failed",
			Reason: verification.FailureReason, ErrorType: verification.FailureReason, At: observedAt,
		}); err != nil {
			return fmt.Errorf("append privd receipt verification audit: %w", err)
		}
	}
	if terminalEvidenceOnly {
		return nil
	}
	operationEventID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate operation result event ID: %w", err)
	}
	terminal := effectiveState == "succeeded" || effectiveState == "failed" || effectiveState == "rejected" || effectiveState == "rolled_back"
	commandTag, err := tx.Exec(ctx, `UPDATE commands SET state=$2,updated_at=GREATEST(updated_at,$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, commandID, effectiveState, observedAt)
	if err != nil {
		return fmt.Errorf("apply Agent command result: %w", err)
	}
	if commandTag.RowsAffected() == 0 {
		return nil
	}
	operationState := effectiveState
	if effectiveState == "rejected" {
		operationState = "failed"
	}
	var operationID, workspaceID uuid.UUID
	var operationRequestID, operationTraceID string
	err = tx.QueryRow(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=GREATEST(updated_at,$3),completed_at=CASE WHEN $4::boolean THEN GREATEST(COALESCE(completed_at,$3),$3) ELSE NULL END WHERE command_id=$1 AND state IN ('queued','dispatched','accepted','running','unknown') RETURNING id,workspace_id,request_id,COALESCE(trace_id,'')`, commandID, operationState, observedAt, terminal).Scan(&operationID, &workspaceID, &operationRequestID, &operationTraceID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apply Agent operation result: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidCommandResult("command result does not match a mutable operation")
	}
	if terminal {
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, commandID, observedAt); err != nil {
			return fmt.Errorf("complete command reconciliation outbox: %w", err)
		}
	}
	if dispatchInFlight {
		attemptTag, err := tx.Exec(ctx, `UPDATE command_attempts
			SET state='sent',finished_at=clock_timestamp()
			WHERE id=$1 AND command_id=$2 AND state='sending' AND finished_at IS NULL`, inFlightAttemptID, commandID)
		if err != nil {
			return fmt.Errorf("close result-observed dispatch attempt: %w", err)
		}
		if attemptTag.RowsAffected() != 1 {
			return errors.New("result-observed dispatch attempt is no longer sending")
		}
		leaseTag, err := tx.Exec(ctx, `DELETE FROM node_command_leases
			WHERE node_id=$1 AND command_id=$2 AND lease_token=$3`, nodeID, commandID, inFlightLeaseToken)
		if err != nil {
			return fmt.Errorf("release result-observed dispatch lease: %w", err)
		}
		if leaseTag.RowsAffected() != 1 {
			return errors.New("result-observed dispatch lease is no longer valid")
		}
	}
	if apply := envelope.GetConfigApply(); apply != nil {
		applyState := operationState
		failureCode := any(nil)
		if result.GetErrorCode() != "" {
			failureCode = result.GetErrorCode()
		}
		if applyResult != nil {
			if applyResult.GetFailureCode() != "" {
				failureCode = applyResult.GetFailureCode()
			}
			if applyResult.GetFailedCritical() {
				applyState = "failed_critical"
			} else if applyResult.GetRolledBack() {
				applyState = "rolled_back"
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE config_apply_operations SET state=$2,failure_code=$3,updated_at=$4 WHERE operation_id=$1`, operationID, applyState, failureCode, observedAt); err != nil {
			return fmt.Errorf("update configuration apply outcome: %w", err)
		}
		if applyState == "succeeded" {
			tag, err := tx.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,candidate_hash,redacted_config,automation_locked,last_apply_operation_id,updated_at)
					SELECT x.node_id,$2,$2,$3,p.candidate_redacted,false,$1,$4 FROM config_apply_operations x JOIN config_plans p ON p.id=x.plan_id WHERE x.operation_id=$1 AND x.node_id=$5
					ON CONFLICT(node_id) DO UPDATE SET revision=EXCLUDED.revision,desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),candidate_hash=EXCLUDED.candidate_hash,redacted_config=EXCLUDED.redacted_config,automation_locked=false,automation_lock_reason=NULL,last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, operationID, apply.GetDesiredRevision(), apply.GetCandidateHash(), observedAt, nodeID)
			if err != nil {
				return fmt.Errorf("advance configuration revision: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return invalidCommandResult("configuration apply result has no immutable plan")
			}
		}
		if applyState == "failed_critical" {
			if _, err := tx.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,automation_locked,automation_lock_reason,last_apply_operation_id,updated_at)
					VALUES($1,0,$4,'',true,'config_apply_rollback_failed',$2,$3) ON CONFLICT(node_id) DO UPDATE SET desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),automation_locked=true,automation_lock_reason='config_apply_rollback_failed',last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, nodeID, operationID, observedAt, apply.GetDesiredRevision()); err != nil {
				return fmt.Errorf("lock configuration automation: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,created_at) VALUES($1,$2,'critical','config_apply.rollback_failed',$3)`, uuid.Must(uuid.NewV7()), workspaceID, observedAt); err != nil {
				return fmt.Errorf("emit configuration rollback alert: %w", err)
			}
		}
	}
	if csr := envelope.GetCertificateCsr(); csr != nil {
		certificateID, parseErr := uuid.FromBytes(csr.GetCertificateId())
		if parseErr != nil {
			return invalidCommandResult("certificate command ID is invalid")
		}
		certificateState := "failed"
		if effectiveState == "unknown" {
			certificateState = "unknown"
		} else if effectiveState == "succeeded" && csrResult != nil {
			certificateState = "csr_ready"
		}
		var csrDER, publicHash any
		if csrResult != nil {
			csrDER, publicHash = csrResult.GetCsrDer(), csrResult.GetPublicKeySha256()
		}
		var receiptVerifiedAt, receiptDigest, receiptKeyID, effectRecordID, csrDigest, subjectDigest any
		if certificateState == "csr_ready" && verification.Verified() && verification.Certificate != nil {
			receiptVerifiedAt, receiptDigest, receiptKeyID = observedAt, verification.ReceiptSHA256, verification.KeyID
			effectRecordID, csrDigest, subjectDigest = verification.EffectRecordID, verification.Certificate.GetCsrDerSha256(), verification.Certificate.GetRequestedSubjectSha256()
		}
		if _, err := tx.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,csr_der=COALESCE($3,csr_der),public_key_sha256=COALESCE($4,public_key_sha256),csr_receipt_verified_at=COALESCE($7,csr_receipt_verified_at),csr_receipt_sha256=COALESCE($8,csr_receipt_sha256),csr_privd_attestation_key_id=COALESCE($9,csr_privd_attestation_key_id),csr_effect_record_id=COALESCE($10,csr_effect_record_id),csr_der_sha256=COALESCE($11,csr_der_sha256),csr_requested_subject_sha256=COALESCE($12,csr_requested_subject_sha256),updated_at=$5 WHERE id=$1 AND operation_id=$6`, certificateID, certificateState, csrDER, publicHash, observedAt, operationID, receiptVerifiedAt, receiptDigest, receiptKeyID, effectRecordID, csrDigest, subjectDigest); err != nil {
			return fmt.Errorf("update certificate CSR outcome: %w", err)
		}
	}
	if revoke := envelope.GetCertificateRevoke(); revoke != nil {
		certificateID, parseErr := uuid.FromBytes(revoke.GetCertificateId())
		if parseErr != nil {
			return invalidCommandResult("certificate revoke ID is invalid")
		}
		certificateState := "revoking"
		var revokedAt any
		if effectiveState == "unknown" {
			certificateState = "unknown"
		} else if effectiveState == "succeeded" && revokeResult != nil {
			certificateState, revokedAt = "revoked", observedAt
		}
		if _, err := tx.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,revoked_at=COALESCE($3,revoked_at),revocation_reason=CASE WHEN $2='revoked' THEN $4 ELSE revocation_reason END,updated_at=$5 WHERE id=$1 AND node_id=$6`, certificateID, certificateState, revokedAt, revoke.GetReason(), observedAt, nodeID); err != nil {
			return fmt.Errorf("update certificate revocation outcome: %w", err)
		}
	}
	if artifact := envelope.GetCertificateP12(); artifact != nil {
		artifactID, parseErr := uuid.FromBytes(artifact.GetArtifactId())
		if parseErr != nil {
			return invalidCommandResult("certificate artifact ID is invalid")
		}
		artifactState := "failed"
		var digest, size any
		if effectiveState == "unknown" {
			artifactState = "pending"
		} else if effectiveState == "succeeded" && artifactResult != nil {
			artifactState, digest, size = "ready", artifactResult.GetArtifactSha256(), int64(artifactResult.GetArtifactSize())
		}
		if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state=$2,content_sha256=COALESCE($3,content_sha256),content_size=COALESCE($4,content_size),updated_at=$5 WHERE id=$1 AND operation_id=$6`, artifactID, artifactState, digest, size, observedAt, operationID); err != nil {
			return fmt.Errorf("update certificate artifact outcome: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,$3,$4)`, operationEventID, operationID, operationState, observedAt); err != nil {
		return fmt.Errorf("append Agent operation result event: %w", err)
	}
	if terminal {
		auditResult := "failed"
		if operationState == "succeeded" {
			auditResult = "succeeded"
		}
		action := commandAuditAction(&envelope)
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "agent", ActorID: envelope.GetActorId(), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &nodeID, CommandID: &commandID, RequestID: operationRequestID, TraceID: operationTraceID, Result: auditResult, Reason: envelope.GetReason(), ErrorType: result.GetErrorCode(), At: observedAt}); err != nil {
			return fmt.Errorf("append Agent audit result: %w", err)
		}
	}
	if effectiveState == "unknown" {
		if err := scheduleCommandRecovery(ctx, tx, commandID, &envelope, recoveryReason, observedAt, signer); err != nil {
			return err
		}
	}
	return nil
}

func invalidCommandResult(detail string) error {
	return invalidEvent("invalid_command_result", detail)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableUint64(value uint64) any {
	if value == 0 {
		return nil
	}
	return int64(value)
}

func normalizeCertificateCSRResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateCsrResult, error) {
	request := envelope.GetCertificateCsr()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateCsrResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil {
		return nil, errors.New("certificate CSR result is malformed")
	}
	if !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || len(result.GetCsrDer()) < 64 || len(result.GetCsrDer()) > 64*1024 || len(result.GetPublicKeySha256()) != sha256.Size {
		return nil, errors.New("certificate CSR result is inconsistent")
	}
	csr, err := x509.ParseCertificateRequest(result.GetCsrDer())
	if err != nil || csr.CheckSignature() != nil {
		return nil, errors.New("certificate CSR signature is invalid")
	}
	key, ok := csr.PublicKey.(*rsa.PublicKey)
	requestedNames, actualNames := slices.Clone(request.GetDnsNames()), slices.Clone(csr.DNSNames)
	sort.Strings(requestedNames)
	sort.Strings(actualNames)
	if !ok || uint32(key.N.BitLen()) != request.GetKeyBits() || csr.Subject.CommonName != request.GetCommonName() || len(csr.Subject.Names) != 1 || !csr.Subject.Names[0].Type.Equal([]int{2, 5, 4, 3}) || !slices.Equal(requestedNames, actualNames) || len(csr.EmailAddresses) != 0 || len(csr.IPAddresses) != 0 || len(csr.URIs) != 0 {
		return nil, errors.New("certificate CSR identity does not match the requested subject")
	}
	for _, extension := range csr.Extensions {
		if !extension.Id.Equal([]int{2, 5, 29, 17}) {
			return nil, errors.New("certificate CSR contains an unsupported extension")
		}
	}
	publicKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, errors.New("certificate CSR public key is invalid")
	}
	digest := sha256.Sum256(publicKey)
	if !bytes.Equal(result.GetPublicKeySha256(), digest[:]) {
		return nil, errors.New("certificate CSR public key digest mismatch")
	}
	return &result, nil
}

func normalizeCertificateRevokeResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateRevokeResult, error) {
	request := envelope.GetCertificateRevoke()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateRevokeResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil || !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || !result.GetKeyRemoved() {
		return nil, errors.New("certificate revoke result is inconsistent")
	}
	return &result, nil
}

func normalizeCertificateArtifactResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (*agentv1.CertificateArtifactResult, error) {
	request := envelope.GetCertificateP12()
	if request == nil || state != "succeeded" {
		return nil, nil
	}
	var result agentv1.CertificateArtifactResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil || !bytes.Equal(result.GetCertificateId(), request.GetCertificateId()) || !bytes.Equal(result.GetArtifactId(), request.GetArtifactId()) || len(result.GetArtifactSha256()) != sha256.Size || result.GetArtifactSize() == 0 || result.GetArtifactSize() > 64*1024*1024 {
		return nil, errors.New("certificate artifact result is inconsistent")
	}
	return &result, nil
}

func normalizeConfigApplyResult(envelope *agentv1.CommandEnvelope, state string, resultBytes []byte) (string, *agentv1.ConfigApplyResult, error) {
	apply := envelope.GetConfigApply()
	if apply == nil || state != "succeeded" {
		return state, nil, nil
	}
	var result agentv1.ConfigApplyResult
	if len(resultBytes) == 0 || proto.Unmarshal(resultBytes, &result) != nil {
		return "", nil, errors.New("configuration apply result is malformed")
	}
	if !bytes.Equal(result.GetCandidateHash(), apply.GetCandidateHash()) || !bytes.Equal(result.GetPreviousHash(), apply.GetExpectedCurrentHash()) {
		return "", nil, errors.New("configuration apply result hash mismatch")
	}
	switch {
	case result.GetHealthy() && !result.GetRolledBack() && !result.GetFailedCritical() && result.GetFailureCode() == "" &&
		bytes.Equal(result.GetObservedHash(), apply.GetCandidateHash()) && result.GetAppliedRevision() == apply.GetDesiredRevision():
		return "succeeded", &result, nil
	case result.GetHealthy() && result.GetRolledBack() && !result.GetFailedCritical() && result.GetAppliedRevision() == 0 &&
		bytes.Equal(result.GetObservedHash(), apply.GetExpectedCurrentHash()) &&
		(result.GetFailureCode() == "health_check_failed" || result.GetFailureCode() == "recovered_health_check_failed"):
		return "rolled_back", &result, nil
	case !result.GetHealthy() && !result.GetRolledBack() && result.GetFailedCritical() && result.GetAppliedRevision() == 0 && len(result.GetObservedHash()) == 0 &&
		(result.GetFailureCode() == "rollback_failed" || result.GetFailureCode() == "recovery_rollback_failed"):
		return "failed", &result, nil
	default:
		return "", nil, errors.New("configuration apply result has an invalid outcome")
	}
}

func commandAuditAction(envelope *agentv1.CommandEnvelope) string {
	switch envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_SessionDisconnect:
		return "session.disconnect"
	case *agentv1.CommandEnvelope_SessionTerminate:
		return "session.terminate"
	case *agentv1.CommandEnvelope_IpBanRemove:
		return "ip_ban.remove"
	case *agentv1.CommandEnvelope_ServiceReload:
		return "service.reload"
	case *agentv1.CommandEnvelope_UserCreate:
		return "user.create"
	case *agentv1.CommandEnvelope_UserDisable:
		return "user.disable"
	case *agentv1.CommandEnvelope_UserEnable:
		return "user.enable"
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		return "user.password.rotate"
	case *agentv1.CommandEnvelope_GroupApply:
		return "group.apply"
	case *agentv1.CommandEnvelope_ConfigPlan:
		return "config.plan"
	case *agentv1.CommandEnvelope_ConfigApply:
		return "config.apply"
	case *agentv1.CommandEnvelope_CertificateCsr:
		return "certificate.csr.generate"
	case *agentv1.CommandEnvelope_CertificateP12:
		return "certificate.private_key.export"
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return "certificate.revoke"
	default:
		return "synthetic.command"
	}
}

func scheduleCommandRecovery(ctx context.Context, tx pgx.Tx, commandID uuid.UUID, envelope *agentv1.CommandEnvelope, reason string, observedAt time.Time, signer *commandauth.Signer) error {
	var mode agentv1.CommandDeliveryMode
	switch reason {
	case "effect_absent":
		if envelope.GetDeliveryMode() != agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY {
			return invalidCommandResult("effect absence was not observed during reconciliation")
		}
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT
	case "outcome_requires_reconciliation", "result_persistence_failed", "privd_transport_unknown", "privd_outcome_unknown":
		mode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	default:
		return nil
	}
	if mode == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RETRY_IF_EFFECT_ABSENT {
		expiresAt := envelope.GetExpiresAt()
		if expiresAt == nil || expiresAt.CheckValid() != nil || !expiresAt.AsTime().After(observedAt) {
			return nil
		}
	}
	payload, expiresAt, err := operationstore.PrepareRecoveryEnvelope(envelope, mode, observedAt, signer)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE commands SET envelope=$2,expires_at=$3 WHERE id=$1 AND state='unknown'`, commandID, payload, expiresAt); err != nil {
		return fmt.Errorf("persist reconciliation command: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE operations SET expires_at=$2 WHERE command_id=$1 AND state='unknown'`, commandID, expiresAt); err != nil {
		return fmt.Errorf("extend reconciliation operation: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error=NULL WHERE command_id=$1`, commandID, payload, observedAt); err != nil {
		return fmt.Errorf("schedule reconciliation command: %w", err)
	}
	return nil
}

func agentPayloadHash(envelope *agentv1.CommandEnvelope) ([sha256.Size]byte, error) {
	var capability string
	var payload []byte
	var err error
	switch {
	case envelope.GetSyntheticNoop() != nil:
		capability = "synthetic.noop"
		payload, err = proto.Marshal(envelope.GetSyntheticNoop())
	case envelope.GetSyntheticEcho() != nil:
		capability = "synthetic.echo"
		payload, err = proto.Marshal(envelope.GetSyntheticEcho())
	default:
		return [sha256.Size]byte{}, errors.New("command result payload type is not reconcilable")
	}
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode command payload for result verification: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(capability))
	_, _ = hash.Write(envelope.GetNodeId())
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], envelope.GetExpectedRevision())
	_, _ = hash.Write(revision[:])
	_, _ = hash.Write(payload)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
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
		ActorId: "developer", Reason: "I03 local side-effect-free slice", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
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
	case transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_SIMULATION_RESULT:
		return "simulation_result", nil
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

func connectedOwnerTerm(event *transportv1.TransportEvent) ([16]byte, bool, error) {
	connection := event.GetConnectionId()
	epoch := event.GetOwnerEpoch()
	if len(connection) == 0 && epoch == 0 {
		return [16]byte{}, false, nil
	}
	if len(connection) != 16 || epoch == 0 || epoch > math.MaxInt64 {
		return [16]byte{}, false, errors.New("connected owner term must contain a 16-byte connection_id and positive owner_epoch")
	}
	id, err := uuid.FromBytes(connection)
	if err != nil || id.Version() != 7 {
		return [16]byte{}, false, errors.New("connected owner connection_id must be UUIDv7")
	}
	var fixed [16]byte
	copy(fixed[:], connection)
	return fixed, true, nil
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
