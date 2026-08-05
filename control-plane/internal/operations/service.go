package operations

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrInvalidRequest      = errors.New("invalid operation request")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")
	ErrStaleRevision       = errors.New("resource revision is stale")
	ErrNodeUnavailable     = errors.New("node is unavailable")
)

type SyntheticKind string

const (
	SyntheticNoop SyntheticKind = "noop"
	SyntheticEcho SyntheticKind = "echo"
)

type CreateRequest struct {
	NodeID           uuid.UUID
	IdempotencyKey   string
	ExpectedVersion  int64
	Kind             SyntheticKind
	Message          string
	SupersedePending bool
	TTL              time.Duration
	RequestID        string
	Traceparent      string
}

type Operation struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	NodeID    *string    `json:"node_id,omitempty"`
	CommandID *string    `json:"command_id,omitempty"`
	Version   int64      `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Event struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	State       string    `json:"state"`
	OccurredAt  time.Time `json:"occurred_at"`
	Sequence    int64     `json:"-"`
}

type Dispatch struct {
	AttemptID   uuid.UUID
	CommandID   uuid.UUID
	OperationID uuid.UUID
	OutboxID    uuid.UUID
	NodeID      uuid.UUID
	LeaseToken  uuid.UUID
	Envelope    []byte
	Traceparent string
}

type QueueMetrics struct {
	Unpublished int64   `json:"outbox_unpublished_total"`
	OldestAge   float64 `json:"outbox_oldest_age_seconds"`
	Queued      int64   `json:"command_queue_depth"`
	Unknown     int64   `json:"command_unknown_total"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) CreateSynthetic(ctx context.Context, request CreateRequest) (Operation, bool, error) {
	if err := validateCreate(request); err != nil {
		return Operation{}, false, err
	}
	now := s.now()
	hash := requestHash(request)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, false, fmt.Errorf("begin operation transaction: %w", err)
	}
	defer rollback(tx)

	var workspaceID uuid.UUID
	var nodeVersion int64
	var nodeStatus string
	if err := tx.QueryRow(ctx, `SELECT workspace_id, version, status FROM nodes WHERE id = $1 FOR UPDATE`, request.NodeID).Scan(&workspaceID, &nodeVersion, &nodeStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, false, ErrNodeUnavailable
		}
		return Operation{}, false, fmt.Errorf("lock operation node: %w", err)
	}
	if nodeStatus != "active" && nodeStatus != "offline" {
		return Operation{}, false, ErrNodeUnavailable
	}
	if existing, same, err := findIdempotent(ctx, tx, workspaceID, request.IdempotencyKey, hash[:]); err != nil {
		return Operation{}, false, err
	} else if existing.ID != "" {
		if !same {
			return Operation{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Operation{}, false, fmt.Errorf("commit idempotent lookup: %w", err)
		}
		return existing, true, nil
	}
	if nodeVersion != request.ExpectedVersion {
		return Operation{}, false, ErrStaleRevision
	}

	operationID, commandID, outboxID, auditID, eventID, err := newIDs(5)
	if err != nil {
		return Operation{}, false, err
	}
	expiresAt := now.Add(request.TTL)
	envelope, payloadType, err := marshalEnvelope(request, operationID, commandID, now, expiresAt)
	if err != nil {
		return Operation{}, false, err
	}
	if request.SupersedePending {
		if err := supersedePending(ctx, tx, request.NodeID, payloadType, now); err != nil {
			return Operation{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations (id, workspace_id, node_id, command_id, state, version, request_id, trace_id, idempotency_key, request_hash, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'queued',1,$5,$6,$7,$8,$9,$10,$10)`,
		operationID, workspaceID, request.NodeID, commandID, request.RequestID, traceID(request.Traceparent), request.IdempotencyKey, hash[:], expiresAt, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation intent: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO commands (id, operation_id, workspace_id, node_id, state, payload_type, envelope, idempotency_key, expected_version, traceparent, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$10,$11,$11)`,
		commandID, operationID, workspaceID, request.NodeID, payloadType, envelope, request.IdempotencyKey, request.ExpectedVersion, request.Traceparent, expiresAt, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert command: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, command_id, event_type, payload, available_at, created_at)
		VALUES ($1,$2,'command.dispatch',$3,$4,$4)`, outboxID, commandID, envelope, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert outbox event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events (id, operation_id, state, occurred_at) VALUES ($1,$2,'queued',$3)`, eventID, operationID, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation event: %w", err)
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "development_stub", ActorID: "developer", Action: "synthetic.command", ResourceType: "operation", ResourceID: operationID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: "side-effect-free delivery validation", At: now}); err != nil {
		return Operation{}, false, fmt.Errorf("append operation audit intent: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox', $1)`, outboxID.String()); err != nil {
		return Operation{}, false, fmt.Errorf("notify outbox worker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, false, fmt.Errorf("commit operation transaction: %w", err)
	}
	nodeText, commandText := request.NodeID.String(), commandID.String()
	return Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiresAt}, false, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	return scanOperation(s.pool.QueryRow(ctx, `SELECT id::text,state,node_id::text,command_id::text,version,created_at,updated_at,expires_at FROM operations WHERE id=$1`, id))
}

func (s *Service) ListEvents(ctx context.Context, operationID, after uuid.UUID, limit int) ([]Event, error) {
	if limit < 1 || limit > 200 {
		return nil, ErrInvalidRequest
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,operation_id::text,state,occurred_at,sequence FROM operation_events WHERE operation_id=$1 AND sequence>COALESCE((SELECT sequence FROM operation_events WHERE id=$2 AND operation_id=$1),0) ORDER BY sequence LIMIT $3`, operationID, nullableUUID(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list operation events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.OperationID, &event.State, &event.OccurredAt, &event.Sequence); err != nil {
			return nil, fmt.Errorf("scan operation event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Service) Claim(ctx context.Context, workerID uuid.UUID, limit int, lease time.Duration) ([]Dispatch, error) {
	if workerID == uuid.Nil || limit < 1 || limit > 100 || lease <= 0 {
		return nil, ErrInvalidRequest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `
		SELECT outbox.id, command.id, command.operation_id, command.node_id, outbox.payload, command.traceparent
		FROM outbox_events AS outbox
		JOIN commands AS command ON command.id=outbox.command_id
		WHERE outbox.published_at IS NULL AND outbox.available_at<=now()
		  AND (outbox.locked_until IS NULL OR outbox.locked_until<=now())
		  AND command.state IN ('queued','unknown') AND command.expires_at>now()
		ORDER BY outbox.available_at,outbox.id FOR UPDATE OF outbox SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("select outbox candidates: %w", err)
	}
	type candidate struct {
		outboxID, commandID, operationID, nodeID uuid.UUID
		payload                                  []byte
		traceparent                              string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.outboxID, &c.commandID, &c.operationID, &c.nodeID, &c.payload, &c.traceparent); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	claimed := make([]Dispatch, 0, len(candidates))
	for _, c := range candidates {
		if _, err := tx.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=$1 AND leased_until<=now()`, c.nodeID); err != nil {
			return nil, err
		}
		attemptID, token, err := twoIDs()
		if err != nil {
			return nil, err
		}
		result, err := tx.Exec(ctx, `INSERT INTO node_command_leases (node_id,command_id,lease_token,worker_id,leased_until,created_at) VALUES ($1,$2,$3,$4,now()+$5::interval,now()) ON CONFLICT DO NOTHING`, c.nodeID, c.commandID, token, workerID, lease.String())
		if err != nil {
			return nil, fmt.Errorf("acquire node lease: %w", err)
		}
		if result.RowsAffected() == 0 {
			continue
		}
		var attempt int
		if err := tx.QueryRow(ctx, `UPDATE outbox_events SET locked_by=$2,locked_until=now()+$3::interval,attempts=attempts+1 WHERE id=$1 RETURNING attempts`, c.outboxID, workerID, lease.String()).Scan(&attempt); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO command_attempts (id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at) VALUES ($1,$2,$3,$4,$5,'sending',now())`, attemptID, c.commandID, c.outboxID, workerID, attempt); err != nil {
			return nil, err
		}
		claimed = append(claimed, Dispatch{AttemptID: attemptID, CommandID: c.commandID, OperationID: c.operationID, OutboxID: c.outboxID, NodeID: c.nodeID, LeaseToken: token, Envelope: c.payload, Traceparent: c.traceparent})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit outbox claim: %w", err)
	}
	return claimed, nil
}

func (s *Service) MarkSent(ctx context.Context, dispatch Dispatch) error {
	return s.finishDispatch(ctx, dispatch, true, "")
}

func (s *Service) MarkFailed(ctx context.Context, dispatch Dispatch, cause error) error {
	message := "transport unavailable"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return s.finishDispatch(ctx, dispatch, false, message)
}

func (s *Service) finishDispatch(ctx context.Context, d Dispatch, sent bool, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_command_leases WHERE node_id=$1 AND command_id=$2 AND lease_token=$3 AND leased_until>now())`, d.NodeID, d.CommandID, d.LeaseToken).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("dispatch lease is no longer valid")
	}
	if sent {
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE id=$1 AND locked_by IS NOT NULL`, d.OutboxID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE commands SET state='dispatched',updated_at=now() WHERE id=$1 AND state IN ('queued','unknown')`, d.CommandID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='dispatched',version=version+1,updated_at=now() WHERE id=$1 AND state='queued'`, d.OperationID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=now() WHERE id=$1 AND state='sending'`, d.AttemptID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events (id,operation_id,state,occurred_at) VALUES ($1,$2,'dispatched',now())`, eventID, d.OperationID); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET locked_by=NULL,locked_until=NULL,available_at=now()+interval '1 second',last_error=$2 WHERE id=$1 AND locked_by IS NOT NULL`, d.OutboxID, message); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE command_attempts SET state='failed',finished_at=now(),error_code='transport_unavailable' WHERE id=$1 AND state='sending'`, d.AttemptID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_command_leases WHERE lease_token=$1`, d.LeaseToken); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Reap(ctx context.Context, maxAttempts int) error {
	if maxAttempts < 1 {
		return ErrInvalidRequest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `UPDATE command_attempts AS attempt SET state='unknown',finished_at=now(),error_code='lease_expired' FROM node_command_leases AS lease WHERE attempt.command_id=lease.command_id AND attempt.state='sending' AND lease.leased_until<=now()`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox_events AS outbox SET locked_by=NULL,locked_until=NULL,available_at=now() FROM node_command_leases AS lease WHERE outbox.command_id=lease.command_id AND lease.leased_until<=now() AND outbox.attempts<$1`, maxAttempts); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE commands AS command SET state='unknown',updated_at=now() FROM outbox_events AS outbox,node_command_leases AS lease WHERE outbox.command_id=command.id AND lease.command_id=command.id AND lease.leased_until<=now() AND outbox.attempts>=$1 AND command.state='queued' RETURNING command.operation_id,outbox.id`, maxAttempts)
	if err != nil {
		return err
	}
	type stopped struct{ operationID, outboxID uuid.UUID }
	var stoppedRows []stopped
	for rows.Next() {
		var row stopped
		if err := rows.Scan(&row.operationID, &row.outboxID); err != nil {
			rows.Close()
			return err
		}
		stoppedRows = append(stoppedRows, row)
	}
	rows.Close()
	for _, row := range stoppedRows {
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,updated_at=now() WHERE id=$1 AND state='queued'`, row.operationID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error='dispatch outcome unknown' WHERE id=$1`, row.outboxID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'unknown',now())`, eventID, row.operationID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_command_leases WHERE leased_until<=now()`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Expire(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin command expiry: %w", err)
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `WITH expired AS (UPDATE commands SET state='expired',updated_at=now() WHERE state='queued' AND expires_at<=now() RETURNING id,operation_id), stopped AS (UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error='command expired before dispatch' FROM expired WHERE outbox_events.command_id=expired.id RETURNING expired.operation_id) UPDATE operations SET state='expired',version=version+1,updated_at=now(),completed_at=now() FROM stopped WHERE operations.id=stopped.operation_id AND operations.state='queued' RETURNING operations.id`)
	if err != nil {
		return fmt.Errorf("expire queued commands: %w", err)
	}
	var operationIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		operationIDs = append(operationIDs, id)
	}
	rows.Close()
	for _, operationID := range operationIDs {
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'expired',now())`, eventID, operationID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) Metrics(ctx context.Context) (QueueMetrics, error) {
	var m QueueMetrics
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE published_at IS NULL),COALESCE(extract(epoch FROM now()-min(created_at) FILTER(WHERE published_at IS NULL)),0),(SELECT count(*) FROM commands WHERE state='queued'),(SELECT count(*) FROM commands WHERE state='unknown') FROM outbox_events`).Scan(&m.Unpublished, &m.OldestAge, &m.Queued, &m.Unknown)
	if err != nil {
		return QueueMetrics{}, fmt.Errorf("read queue metrics: %w", err)
	}
	return m, nil
}

func validateCreate(r CreateRequest) error {
	if r.NodeID == uuid.Nil || r.NodeID.Version() != 7 || r.ExpectedVersion < 1 || r.RequestID == "" || !validTraceparent(r.Traceparent) || r.TTL < time.Second || r.TTL > 24*time.Hour {
		return ErrInvalidRequest
	}
	if len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || strings.TrimSpace(r.IdempotencyKey) != r.IdempotencyKey {
		return ErrInvalidRequest
	}
	if r.Kind != SyntheticNoop && r.Kind != SyntheticEcho {
		return ErrInvalidRequest
	}
	if r.Kind == SyntheticNoop && r.Message != "" || len(r.Message) > 4096 {
		return ErrInvalidRequest
	}
	return nil
}

func requestHash(r CreateRequest) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%t\x00%d", r.Kind, r.Message, r.ExpectedVersion, r.SupersedePending, r.TTL/time.Second)))
}

func marshalEnvelope(r CreateRequest, operationID, commandID uuid.UUID, now, expires time.Time) ([]byte, string, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, "", err
	}
	envelope := &agentv1.CommandEnvelope{ProtocolVersion: "1.0", MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: r.NodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), ExpectedRevision: uint64(r.ExpectedVersion), Traceparent: r.Traceparent, ActorId: "developer", Reason: "side-effect-free delivery validation", DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
	payloadType := "synthetic_noop"
	if r.Kind == SyntheticEcho {
		payloadType = "synthetic_echo"
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: r.Message}}
	} else {
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	}
	if err := semanticpayload.PopulateV1(envelope); err != nil {
		return nil, "", fmt.Errorf("compute semantic payload hash: %w", err)
	}
	data, err := proto.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("marshal typed command: %w", err)
	}
	return data, payloadType, nil
}

func findIdempotent(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, key string, hash []byte) (Operation, bool, error) {
	row := tx.QueryRow(ctx, `SELECT id::text,state,node_id::text,command_id::text,version,created_at,updated_at,expires_at,request_hash=$3 FROM operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, key, hash)
	var op Operation
	var nodeID, commandID *string
	var same bool
	err := row.Scan(&op.ID, &op.State, &nodeID, &commandID, &op.Version, &op.CreatedAt, &op.UpdatedAt, &op.ExpiresAt, &same)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, fmt.Errorf("read idempotent operation: %w", err)
	}
	op.NodeID = nodeID
	op.CommandID = commandID
	return op, same, nil
}

func scanOperation(row pgx.Row) (Operation, error) {
	var op Operation
	var nodeID, commandID *string
	err := row.Scan(&op.ID, &op.State, &nodeID, &commandID, &op.Version, &op.CreatedAt, &op.UpdatedAt, &op.ExpiresAt)
	if err != nil {
		return Operation{}, err
	}
	op.NodeID = nodeID
	op.CommandID = commandID
	return op, nil
}

func supersedePending(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, payloadType string, now time.Time) error {
	rows, err := tx.Query(ctx, `UPDATE commands AS command SET state='superseded',updated_at=$3 FROM outbox_events AS outbox WHERE command.node_id=$1 AND command.payload_type=$2 AND command.state='queued' AND outbox.command_id=command.id AND outbox.locked_by IS NULL AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id) RETURNING command.operation_id,command.id`, nodeID, payloadType, now)
	if err != nil {
		return err
	}
	type old struct{ operationID, commandID uuid.UUID }
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.operationID, &o.commandID); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, o)
	}
	rows.Close()
	for _, o := range olds {
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1 AND state='queued'`, o.operationID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=$2,last_error='superseded by newer intent' WHERE command_id=$1`, o.commandID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'superseded',$3)`, eventID, o.operationID, now); err != nil {
			return err
		}
	}
	return nil
}

func newIDs(count int) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	ids := make([]uuid.UUID, count)
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, uuid.Nil, err
		}
		ids[i] = id
	}
	return ids[0], ids[1], ids[2], ids[3], ids[4], nil
}
func twoIDs() (uuid.UUID, uuid.UUID, error) {
	a, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	b, err := uuid.NewV7()
	return a, b, err
}
func traceID(value string) string { return value[3:35] }
func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
func validTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, p := range parts[1:] {
		for _, c := range p {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
