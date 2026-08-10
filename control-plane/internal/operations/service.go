package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
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
	ErrConfigApplyActive   = errors.New("a configuration apply is already active for this node")
	ErrCapabilityMissing   = errors.New("node capability is unavailable")
	ErrTargetNotObserved   = errors.New("target is not present in observed state")
	ErrBacklogExceeded     = commandlimit.ErrBacklogExceeded
)

type SyntheticKind string

const (
	SyntheticNoop     SyntheticKind = "noop"
	SyntheticEcho     SyntheticKind = "echo"
	SessionDisconnect SyntheticKind = "session_disconnect"
	SessionTerminate  SyntheticKind = "session_terminate"
	IPBanRemove       SyntheticKind = "ip_ban_remove"
	ServiceReload     SyntheticKind = "service_reload"
	ConfigPlan        SyntheticKind = "config_plan"
	ConfigApply       SyntheticKind = "config_apply"
	CertificateCSR    SyntheticKind = "certificate_csr"
	CertificateP12    SyntheticKind = "certificate_p12"
	CertificateRevoke SyntheticKind = "certificate_revoke"
)

type CreateRequest struct {
	NodeID              uuid.UUID
	IdempotencyKey      string
	ExpectedVersion     int64
	Kind                SyntheticKind
	Message             string
	Candidate           []byte
	CandidateHash       []byte
	ExpectedCurrentHash []byte
	DesiredRevision     uint64
	PlanRevision        uint64
	PlanMetadata        *ConfigPlanMetadata
	ApplyMetadata       *ConfigApplyMetadata
	ArtifactMetadata    *ArtifactMetadata
	OcservVersion       string
	PlanCapabilities    []string
	CertificateID       uuid.UUID
	CommonName          string
	DNSNames            []string
	KeyBits             uint32
	CertificateChain    []byte
	SealedPassword      []byte
	SecretKeyID         string
	ArtifactID          uuid.UUID
	RevocationReason    string
	SessionID           string
	BootID              string
	IP                  string
	ActorID             string
	ActorIdentityID     uuid.UUID
	ActorSessionID      uuid.UUID
	ApprovalID          uuid.UUID
	ApprovalRequestHash []byte
	Action              string
	Reason              string
	SupersedePending    bool
	HoldDispatch        bool
	TTL                 time.Duration
	RequestID           string
	Traceparent         string
}

// ConfigPlanMetadata is written atomically with the remote validation intent.
type ConfigPlanMetadata struct {
	TemplateName      string
	CandidateRedacted string
	Warnings          []string
	CreatedBy         uuid.UUID
}

// ConfigApplyMetadata binds an approved immutable plan to its dispatch intent.
type ConfigApplyMetadata struct {
	PlanID uuid.UUID
}

type ArtifactMetadata struct {
	TokenSHA256 []byte
	RequestHash []byte
	ExpiresAt   time.Time
}

type Operation struct {
	ID                     string     `json:"id"`
	State                  string     `json:"state"`
	NodeID                 *string    `json:"node_id,omitempty"`
	CommandID              *string    `json:"command_id,omitempty"`
	ConfigApplyState       string     `json:"config_apply_state,omitempty"`
	ConfigApplyFailureCode string     `json:"config_apply_failure_code,omitempty"`
	Version                int64      `json:"version"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
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
	Unpublished          int64   `json:"outbox_unpublished_total"`
	OldestAge            float64 `json:"outbox_oldest_age_seconds"`
	Queued               int64   `json:"command_queue_depth"`
	Unknown              int64   `json:"command_unknown_total"`
	ConfigRollbacks      int64   `json:"config_rollback_total"`
	ConfigFailedCritical int64   `json:"config_failed_critical_total"`
}

type Service struct {
	pool         *pgxpool.Pool
	now          func() time.Time
	commandLimit int
	signer       *commandauth.Signer
}

func New(pool *pgxpool.Pool) *Service {
	return NewWithConcurrency(pool, 50)
}

func NewWithConcurrency(pool *pgxpool.Pool, commandLimit int) *Service {
	return &Service{pool: pool, now: func() time.Time { return time.Now().UTC() }, commandLimit: commandLimit}
}

// NewWithSigner configures command issuance with an end-to-end Controller signer.
func NewWithSigner(pool *pgxpool.Pool, commandLimit int, signer *commandauth.Signer) *Service {
	service := NewWithConcurrency(pool, commandLimit)
	service.signer = signer
	return service
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
	var authorizationRevision uint64
	var nodeStatus string
	if err := tx.QueryRow(ctx, `SELECT workspace_id, version, authorization_revision, status FROM nodes WHERE id = $1 FOR UPDATE`, request.NodeID).Scan(&workspaceID, &nodeVersion, &authorizationRevision, &nodeStatus); err != nil {
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
	if request.Kind == ConfigPlan {
		var configRevision int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM node_config_state WHERE node_id=$1),0)`, request.NodeID).Scan(&configRevision); err != nil {
			return Operation{}, false, fmt.Errorf("read configuration revision: %w", err)
		}
		if configRevision < 0 || uint64(configRevision) != request.PlanRevision {
			return Operation{}, false, ErrStaleRevision
		}
		var observedVersion string
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT o.ocserv_version FROM node_observed_snapshots o WHERE o.node_id=$1 ORDER BY o.observed_at DESC LIMIT 1),'')`, request.NodeID).Scan(&observedVersion); err != nil {
			return Operation{}, false, fmt.Errorf("read observed Ocserv version: %w", err)
		}
		if observedVersion != request.OcservVersion {
			return Operation{}, false, ErrStaleRevision
		}
		for _, required := range request.PlanCapabilities {
			var approved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability=$2 AND approved=true)`, request.NodeID, required).Scan(&approved); err != nil {
				return Operation{}, false, fmt.Errorf("recheck configuration capability: %w", err)
			}
			if !approved {
				return Operation{}, false, ErrCapabilityMissing
			}
		}
	}
	if request.Kind == ConfigApply {
		var planWorkspace, planNode uuid.UUID
		var planRevision int64
		var planHash, resultBytes []byte
		var planExpires time.Time
		var planState string
		err := tx.QueryRow(ctx, `SELECT p.workspace_id,p.node_id,p.expected_revision,p.candidate_hash,p.expires_at,o.state,
			COALESCE((SELECT r.result FROM agent_command_results r WHERE r.command_id=o.command_id AND r.state='succeeded' ORDER BY r.created_at DESC LIMIT 1),''::bytea)
			FROM config_plans p JOIN operations o ON o.id=p.operation_id WHERE p.id=$1`, request.ApplyMetadata.PlanID).
			Scan(&planWorkspace, &planNode, &planRevision, &planHash, &planExpires, &planState, &resultBytes)
		if err != nil {
			return Operation{}, false, fmt.Errorf("lock configuration plan: %w", err)
		}
		var validation agentv1.ConfigPlanResult
		if planWorkspace != workspaceID || planNode != request.NodeID || planRevision < 0 || uint64(planRevision) != request.PlanRevision || !bytes.Equal(planHash, request.CandidateHash) || !planExpires.After(now) || planState != "succeeded" || proto.Unmarshal(resultBytes, &validation) != nil || !validation.GetCurrentUnchanged() || !validation.GetStagingCleaned() || !bytes.Equal(validation.GetCandidateHash(), planHash) || !bytes.Equal(validation.GetCurrentHash(), request.ExpectedCurrentHash) {
			return Operation{}, false, ErrStaleRevision
		}
		var revision, highestDesiredRevision int64
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM node_config_state WHERE node_id=$1),0),COALESCE((SELECT desired_revision FROM node_config_state WHERE node_id=$1),0),COALESCE((SELECT automation_locked FROM node_config_state WHERE node_id=$1),false)`, request.NodeID).Scan(&revision, &highestDesiredRevision, &locked); err != nil {
			return Operation{}, false, fmt.Errorf("read configuration apply fence: %w", err)
		}
		if locked || revision != planRevision || highestDesiredRevision < revision || request.DesiredRevision != uint64(highestDesiredRevision)+1 {
			return Operation{}, false, ErrStaleRevision
		}
		var applyActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM config_apply_operations WHERE node_id=$1 AND state IN('queued','dispatched','accepted','running','unknown'))`, request.NodeID).Scan(&applyActive); err != nil {
			return Operation{}, false, fmt.Errorf("check active configuration apply: %w", err)
		}
		if applyActive {
			return Operation{}, false, ErrConfigApplyActive
		}
		if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, workspaceID, request.ActorIdentityID, "config.apply", "config_plan", request.ApplyMetadata.PlanID, planHash); err != nil {
			return Operation{}, false, err
		}
		request.ApprovalRequestHash = append([]byte(nil), planHash...)
	}
	if capability := capabilityFor(request.Kind); capability != "" {
		var approved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability=$2 AND approved=true)`, request.NodeID, capability).Scan(&approved); err != nil {
			return Operation{}, false, fmt.Errorf("check operation capability: %w", err)
		}
		if !approved {
			return Operation{}, false, ErrCapabilityMissing
		}
	}
	if request.Kind == SessionDisconnect || request.Kind == SessionTerminate {
		var present bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sessions s JOIN node_observed_snapshots o ON o.node_id=s.node_id WHERE s.node_id=$1 AND s.session_id=$2 AND o.boot_id=$3)`, request.NodeID, request.SessionID, request.BootID).Scan(&present); err != nil {
			return Operation{}, false, fmt.Errorf("check observed session: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if request.Kind == IPBanRemove {
		var present bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_ip_bans WHERE node_id=$1 AND ip=$2::inet)`, request.NodeID, request.IP).Scan(&present); err != nil {
			return Operation{}, false, fmt.Errorf("check observed IP ban: %w", err)
		}
		if !present {
			return Operation{}, false, ErrTargetNotObserved
		}
	}
	if request.Kind == ServiceReload {
		if err := approvals.Consume(ctx, tx, request.ApprovalID, workspaceID, request.ActorIdentityID, request.Action, "node", request.NodeID); err != nil {
			return Operation{}, false, err
		}
	}

	operationID, commandID, outboxID, auditID, eventID, err := newIDs(5)
	if err != nil {
		return Operation{}, false, err
	}
	expiresAt := now.Add(request.TTL)
	envelope, payloadType, err := marshalEnvelope(request, operationID, commandID, authorizationRevision, now, expiresAt, s.signer)
	if err != nil {
		return Operation{}, false, err
	}
	if request.SupersedePending {
		if err := supersedePending(ctx, tx, request.NodeID, payloadType, now); err != nil {
			return Operation{}, false, err
		}
	}
	if err := commandlimit.ReserveBacklog(ctx, tx, workspaceID, request.NodeID); err != nil {
		return Operation{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations (id, workspace_id, node_id, command_id, state, version, request_id, trace_id, idempotency_key, request_hash, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'queued',1,$5,$6,$7,$8,$9,$10,$10)`,
		operationID, workspaceID, request.NodeID, commandID, request.RequestID, traceID(request.Traceparent), request.IdempotencyKey, hash[:], expiresAt, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation intent: %w", err)
	}
	if request.Kind == ConfigPlan {
		warnings, err := json.Marshal(request.PlanMetadata.Warnings)
		if err != nil {
			return Operation{}, false, fmt.Errorf("marshal configuration plan warnings: %w", err)
		}
		createdBy := any(request.PlanMetadata.CreatedBy)
		if request.PlanMetadata.CreatedBy == uuid.Nil {
			createdBy = nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO config_plans(id,workspace_id,node_id,operation_id,template_name,expected_revision,candidate_hash,candidate_redacted,warnings,expires_at,created_by,created_at)
			VALUES($1,$2,$3,$1,$4,$5,$6,$7,$8,$9,$10,$11)`, operationID, workspaceID, request.NodeID, request.PlanMetadata.TemplateName, request.PlanRevision, request.CandidateHash, request.PlanMetadata.CandidateRedacted, warnings, expiresAt, createdBy, now); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration plan: %w", err)
		}
	}
	if request.Kind == ConfigApply {
		if _, err := tx.Exec(ctx, `INSERT INTO config_apply_operations(operation_id,workspace_id,node_id,plan_id,approval_id,expected_revision,desired_revision,candidate_hash,previous_hash,state,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$10)`, operationID, workspaceID, request.NodeID, request.ApplyMetadata.PlanID, request.ApprovalID, request.PlanRevision, request.DesiredRevision, request.CandidateHash, request.ExpectedCurrentHash, now); err != nil {
			return Operation{}, false, fmt.Errorf("insert configuration apply: %w", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,updated_at) VALUES($1,0,$2,'',$3)
			ON CONFLICT(node_id) DO UPDATE SET desired_revision=EXCLUDED.desired_revision,updated_at=EXCLUDED.updated_at
			WHERE node_config_state.desired_revision=$2-1`, request.NodeID, request.DesiredRevision, now)
		if err != nil {
			return Operation{}, false, fmt.Errorf("advance configuration desired revision: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return Operation{}, false, ErrStaleRevision
		}
	}
	if request.Kind == CertificateCSR {
		dnsNames, err := json.Marshal(request.DNSNames)
		if err != nil {
			return Operation{}, false, fmt.Errorf("marshal certificate DNS names: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'csr_pending',$8,$8)`, request.CertificateID, workspaceID, request.NodeID, operationID, request.CommonName, dnsNames, request.KeyBits, now); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate request: %w", err)
		}
	}
	if request.Kind == CertificateP12 {
		if _, err := tx.Exec(ctx, `INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'certificate_p12','pending',$6,$7,$8,$9,$9)`, request.ArtifactID, workspaceID, request.NodeID, request.CertificateID, operationID, request.ArtifactMetadata.TokenSHA256, request.ArtifactMetadata.RequestHash, request.ArtifactMetadata.ExpiresAt, now); err != nil {
			return Operation{}, false, fmt.Errorf("insert certificate artifact operation: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO commands (id, operation_id, workspace_id, node_id, state, payload_type, envelope, idempotency_key, expected_version, traceparent, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$10,$11,$11)`,
		commandID, operationID, workspaceID, request.NodeID, payloadType, envelope, request.IdempotencyKey, request.ExpectedVersion, request.Traceparent, expiresAt, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert command: %w", err)
	}
	availableAt := now
	if request.HoldDispatch {
		availableAt = expiresAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, command_id, event_type, payload, available_at, created_at)
		VALUES ($1,$2,'command.dispatch',$3,$4,$5)`, outboxID, commandID, envelope, availableAt, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert outbox event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events (id, operation_id, state, occurred_at) VALUES ($1,$2,'queued',$3)`, eventID, operationID, now); err != nil {
		return Operation{}, false, fmt.Errorf("insert operation event: %w", err)
	}
	actorID, action, reason := normalizedAuditIntent(request)
	var auditSummary json.RawMessage
	if request.Kind == ConfigPlan {
		auditSummary, _ = json.Marshal(map[string]any{"candidate_hash": fmt.Sprintf("%x", request.CandidateHash), "expected_revision": request.PlanRevision})
	}
	if request.Kind == ConfigApply {
		auditSummary, _ = json.Marshal(map[string]any{"plan_id": request.ApplyMetadata.PlanID, "candidate_hash": fmt.Sprintf("%x", request.CandidateHash), "previous_hash": fmt.Sprintf("%x", request.ExpectedCurrentHash), "desired_revision": request.DesiredRevision})
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{EventID: auditID, WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID, SessionID: optionalUUID(request.ActorSessionID), Action: action, ResourceType: "operation", ResourceID: operationID, NodeID: &request.NodeID, CommandID: &commandID, ApprovalID: optionalUUID(request.ApprovalID), RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: reason, AfterSummary: auditSummary, At: now}); err != nil {
		return Operation{}, false, fmt.Errorf("append operation audit intent: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox', $1)`, outboxID.String()); err != nil {
		return Operation{}, false, fmt.Errorf("notify outbox worker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, false, fmt.Errorf("commit operation transaction: %w", err)
	}
	nodeText, commandText := request.NodeID.String(), commandID.String()
	operation := Operation{ID: operationID.String(), State: "queued", NodeID: &nodeText, CommandID: &commandText, Version: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiresAt}
	if request.Kind == ConfigApply {
		operation.ConfigApplyState = "queued"
	}
	return operation, false, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	return scanOperation(s.pool.QueryRow(ctx, `SELECT o.id::text,o.state,o.node_id::text,o.command_id::text,o.version,o.created_at,o.updated_at,o.expires_at,COALESCE(x.state,''),COALESCE(x.failure_code,'') FROM operations o LEFT JOIN config_apply_operations x ON x.operation_id=o.id WHERE o.id=$1`, id))
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
	available, err := commandlimit.Available(ctx, tx, s.commandLimit)
	if err != nil {
		return nil, fmt.Errorf("reserve global dispatch capacity: %w", err)
	}
	rows, err := tx.Query(ctx, `
		WITH ranked AS (
		  SELECT outbox.id AS outbox_id,command.id AS command_id,command.operation_id,command.node_id,
		         outbox.payload,command.traceparent,command.state,outbox.available_at,
		         row_number() OVER(PARTITION BY command.node_id ORDER BY CASE WHEN command.state='unknown' THEN 0 ELSE 1 END,outbox.available_at,outbox.id) AS node_rank
		  FROM outbox_events AS outbox
		  JOIN commands AS command ON command.id=outbox.command_id
		  JOIN nodes AS node ON node.id=command.node_id AND node.status='active'
		  WHERE outbox.published_at IS NULL AND outbox.available_at<=now()
		    AND (outbox.locked_until IS NULL OR outbox.locked_until<=now())
		    AND command.state IN ('queued','unknown') AND command.expires_at>now()
		    AND NOT EXISTS(SELECT 1 FROM node_command_leases lease WHERE lease.node_id=command.node_id)
		    AND (command.resource_type IS NULL OR command.resource_key IS NULL OR NOT EXISTS (
		      SELECT 1 FROM commands AS prior
		      WHERE prior.node_id=command.node_id AND prior.resource_type=command.resource_type
		        AND prior.resource_key=command.resource_key AND prior.id<>command.id
		        AND prior.expected_version<command.expected_version
		        AND prior.state IN ('queued','dispatched','accepted','running','unknown')
		    ))
		), unknown_candidates AS (
		  SELECT * FROM ranked WHERE node_rank=1 AND state='unknown'
		  ORDER BY available_at,outbox_id LIMIT $1
		), queued_candidates AS (
		  SELECT * FROM ranked WHERE node_rank=1 AND state='queued'
		  ORDER BY available_at,outbox_id
		  LIMIT LEAST($2,GREATEST(0,$1-(SELECT count(*) FROM unknown_candidates)))
		), selected AS (
		  SELECT * FROM unknown_candidates UNION ALL SELECT * FROM queued_candidates
		)
		SELECT outbox.id, command.id, command.operation_id, command.node_id, outbox.payload, command.traceparent
		FROM selected JOIN outbox_events AS outbox ON outbox.id=selected.outbox_id
		JOIN commands AS command ON command.id=selected.command_id
		ORDER BY CASE WHEN selected.state='unknown' THEN 0 ELSE 1 END,selected.available_at,selected.outbox_id
		FOR UPDATE OF outbox SKIP LOCKED`, limit, available)
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
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return fmt.Errorf("serialize dispatch completion: %w", err)
	}
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
		if _, err = tx.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched',updated_at=now() WHERE operation_id=$1 AND state IN('queued','unknown')`, d.OperationID); err != nil {
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
	if err := commandlimit.Lock(ctx, tx); err != nil {
		return fmt.Errorf("serialize dispatch lease reaping: %w", err)
	}
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
		if _, err = tx.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=now() WHERE operation_id=$1 AND state='queued'`, row.operationID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error='dispatch outcome unknown' WHERE id=$1`, row.outboxID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'unknown',now())`, eventID, row.operationID); err != nil {
			return err
		}
	}
	expiredRows, err := tx.Query(ctx, `SELECT command.id,command.operation_id,command.envelope FROM commands AS command JOIN config_apply_operations AS apply ON apply.operation_id=command.operation_id WHERE command.state IN('dispatched','accepted','running') AND apply.state IN('dispatched','accepted','running') AND command.expires_at<=now() FOR UPDATE OF command`)
	if err != nil {
		return fmt.Errorf("select config applies with missing outcomes: %w", err)
	}
	type expiredApply struct {
		commandID, operationID uuid.UUID
		envelope               []byte
	}
	var expiredApplies []expiredApply
	for expiredRows.Next() {
		var row expiredApply
		if err := expiredRows.Scan(&row.commandID, &row.operationID, &row.envelope); err != nil {
			expiredRows.Close()
			return err
		}
		expiredApplies = append(expiredApplies, row)
	}
	expiredRows.Close()
	for _, row := range expiredApplies {
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(row.envelope, &envelope); err != nil {
			return fmt.Errorf("decode expired configuration apply: %w", err)
		}
		now := s.now()
		deadline := now.Add(5 * time.Minute)
		messageID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		eventID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		envelope.MessageId = messageID[:]
		envelope.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
		envelope.ExpiresAt = timestamppb.New(deadline)
		if err := s.signer.Authorize(&envelope); err != nil {
			return fmt.Errorf("authorize configuration apply reconciliation: %w", err)
		}
		payload, err := proto.Marshal(&envelope)
		if err != nil {
			return fmt.Errorf("encode expired configuration apply reconciliation: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE commands SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state IN('dispatched','accepted','running')`, row.commandID, payload, deadline, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=$2,updated_at=$3 WHERE id=$1 AND state IN('dispatched','accepted','running')`, row.operationID, deadline, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=$2 WHERE operation_id=$1 AND state IN('dispatched','accepted','running')`, row.operationID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='apply outcome missing; reconciliation required' WHERE command_id=$1`, row.commandID, payload, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,'unknown',$3)`, eventID, row.operationID, now); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE config_apply_operations SET state='expired',updated_at=now() WHERE operation_id=$1 AND state='queued'`, operationID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) Metrics(ctx context.Context) (QueueMetrics, error) {
	var m QueueMetrics
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE published_at IS NULL),COALESCE(extract(epoch FROM now()-min(created_at) FILTER(WHERE published_at IS NULL)),0),(SELECT count(*) FROM commands WHERE state='queued'),(SELECT count(*) FROM commands WHERE state='unknown'),(SELECT count(*) FROM config_apply_operations WHERE state='rolled_back'),(SELECT count(*) FROM config_apply_operations WHERE state='failed_critical') FROM outbox_events`).Scan(&m.Unpublished, &m.OldestAge, &m.Queued, &m.Unknown, &m.ConfigRollbacks, &m.ConfigFailedCritical)
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
	if len(r.ApprovalRequestHash) != 0 {
		return ErrInvalidRequest
	}
	if r.Kind != SyntheticNoop && r.Kind != SyntheticEcho && r.Kind != SessionDisconnect && r.Kind != SessionTerminate && r.Kind != IPBanRemove && r.Kind != ServiceReload && r.Kind != ConfigPlan && r.Kind != ConfigApply && r.Kind != CertificateCSR && r.Kind != CertificateP12 && r.Kind != CertificateRevoke {
		return ErrInvalidRequest
	}
	if r.Kind == SyntheticNoop && r.Message != "" || len(r.Message) > 4096 {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan && (len(r.Candidate) == 0 || len(r.Candidate) > 256*1024 || len(r.CandidateHash) != sha256.Size || len(r.ExpectedCurrentHash) != 0 || r.DesiredRevision != 0 || r.ApplyMetadata != nil || r.PlanRevision > uint64(^uint64(0)>>1) || r.PlanMetadata == nil || strings.TrimSpace(r.PlanMetadata.TemplateName) == "" || len(r.PlanMetadata.TemplateName) > 128 || len(r.PlanMetadata.CandidateRedacted) == 0 || len(r.PlanMetadata.CandidateRedacted) > 256*1024 || len(r.PlanCapabilities) == 0) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan {
		digest := sha256.Sum256(r.Candidate)
		if !bytes.Equal(digest[:], r.CandidateHash) {
			return ErrInvalidRequest
		}
	}
	if r.Kind == ConfigApply && (len(r.Candidate) == 0 || len(r.Candidate) > 256*1024 || len(r.CandidateHash) != sha256.Size || len(r.ExpectedCurrentHash) != sha256.Size || r.PlanRevision > uint64(^uint64(0)>>1) || r.DesiredRevision <= r.PlanRevision || r.ApplyMetadata == nil || r.ApplyMetadata.PlanID == uuid.Nil) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigApply {
		digest := sha256.Sum256(r.Candidate)
		if !bytes.Equal(digest[:], r.CandidateHash) || r.ActorID == "" || r.ActorIdentityID == uuid.Nil || r.ActorSessionID == uuid.Nil || r.ApprovalID == uuid.Nil {
			return ErrInvalidRequest
		}
	}
	if r.Kind == CertificateCSR {
		if r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || !validDNSName(r.CommonName) || len(r.DNSNames) > 32 || (r.KeyBits != 2048 && r.KeyBits != 3072 && r.KeyBits != 4096) {
			return ErrInvalidRequest
		}
		for _, name := range r.DNSNames {
			if !validDNSName(name) {
				return ErrInvalidRequest
			}
		}
	}
	if r.Kind == CertificateP12 && (r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || r.ArtifactID == uuid.Nil || r.ArtifactID.Version() != 7 || len(r.CertificateChain) < 64 || len(r.CertificateChain) > 256*1024 || len(r.SealedPassword) < 32 || len(r.SealedPassword) > 16*1024 || strings.TrimSpace(r.SecretKeyID) == "" || len(r.SecretKeyID) > 128 || r.ArtifactMetadata == nil || len(r.ArtifactMetadata.TokenSHA256) != sha256.Size || len(r.ArtifactMetadata.RequestHash) != sha256.Size || !r.ArtifactMetadata.ExpiresAt.After(time.Now().UTC()) || r.ArtifactMetadata.ExpiresAt.After(time.Now().UTC().Add(30*time.Minute))) {
		return ErrInvalidRequest
	}
	if r.Kind == CertificateRevoke && (r.CertificateID == uuid.Nil || r.CertificateID.Version() != 7 || strings.TrimSpace(r.RevocationReason) == "" || len(r.RevocationReason) > 128) {
		return ErrInvalidRequest
	}
	if r.HoldDispatch && r.Kind != CertificateRevoke {
		return ErrInvalidRequest
	}
	if r.Kind != ConfigPlan && r.Kind != ConfigApply && (len(r.Candidate) != 0 || len(r.CandidateHash) != 0 || len(r.ExpectedCurrentHash) != 0 || r.DesiredRevision != 0 || r.PlanRevision != 0 || r.PlanMetadata != nil || r.ApplyMetadata != nil || r.OcservVersion != "" || len(r.PlanCapabilities) != 0) {
		return ErrInvalidRequest
	}
	if r.Kind == SessionDisconnect || r.Kind == SessionTerminate {
		sessionID, err := strconv.ParseUint(r.SessionID, 10, 64)
		if err != nil || sessionID == 0 || strconv.FormatUint(sessionID, 10) != r.SessionID {
			return ErrInvalidRequest
		}
		bootID, err := uuid.Parse(r.BootID)
		if err != nil || bootID == uuid.Nil {
			return ErrInvalidRequest
		}
	}
	if r.Kind == IPBanRemove {
		parsed := net.ParseIP(r.IP)
		if parsed == nil || parsed.String() != r.IP {
			return ErrInvalidRequest
		}
	}
	if (r.Kind == SessionDisconnect || r.Kind == SessionTerminate || r.Kind == IPBanRemove || r.Kind == ServiceReload || r.Kind == ConfigPlan || r.Kind == ConfigApply || r.Kind == CertificateCSR || r.Kind == CertificateP12 || r.Kind == CertificateRevoke) && (r.Action == "" || r.Reason == "" || len(r.Reason) > 512) {
		return ErrInvalidRequest
	}
	if r.Kind == ConfigPlan && r.ActorID == "" {
		return ErrInvalidRequest
	}
	if r.Kind == ServiceReload && (r.ActorID == "" || r.ActorIdentityID == uuid.Nil || r.ActorSessionID == uuid.Nil || r.ApprovalID == uuid.Nil) {
		return ErrInvalidRequest
	}
	return nil
}

func requestHash(r CreateRequest) [32]byte {
	actorID, action, reason := normalizedAuditIntent(r)
	// Request and trace IDs identify an attempt, not the mutation intent. Every
	// field that selects the target, effect, authorization action, actor, audit
	// reason, revision, or delivery behavior is deliberately bound here.
	intent := struct {
		NodeID               uuid.UUID     `json:"node_id"`
		Kind                 SyntheticKind `json:"kind"`
		Message              string        `json:"message"`
		SessionID            string        `json:"session_id"`
		BootID               string        `json:"boot_id"`
		IP                   string        `json:"ip"`
		ExpectedVersion      int64         `json:"expected_version"`
		SupersedePending     bool          `json:"supersede_pending"`
		HoldDispatch         bool          `json:"hold_dispatch,omitempty"`
		TTLSeconds           int64         `json:"ttl_seconds"`
		ActorID              string        `json:"actor_id"`
		Action               string        `json:"action"`
		Reason               string        `json:"reason"`
		ActorSessionID       uuid.UUID     `json:"actor_session_id"`
		ActorIdentityID      uuid.UUID     `json:"actor_identity_id"`
		ApprovalID           uuid.UUID     `json:"approval_id"`
		CandidateHash        string        `json:"candidate_hash"`
		ExpectedCurrentHash  string        `json:"expected_current_hash"`
		DesiredRevision      uint64        `json:"desired_revision"`
		PlanID               uuid.UUID     `json:"plan_id"`
		PlanRevision         uint64        `json:"plan_revision"`
		PlanTemplate         string        `json:"plan_template"`
		OcservVersion        string        `json:"ocserv_version"`
		PlanCapabilities     []string      `json:"plan_capabilities"`
		CertificateID        uuid.UUID     `json:"certificate_id"`
		CommonName           string        `json:"common_name"`
		DNSNames             []string      `json:"dns_names"`
		KeyBits              uint32        `json:"key_bits"`
		ArtifactID           uuid.UUID     `json:"artifact_id"`
		CertificateChainHash string        `json:"certificate_chain_hash"`
		SealedPasswordHash   string        `json:"sealed_password_hash"`
		SecretKeyID          string        `json:"secret_key_id"`
		ArtifactTokenHash    string        `json:"artifact_token_hash"`
		ArtifactRequestHash  string        `json:"artifact_request_hash"`
		ArtifactExpiresAt    string        `json:"artifact_expires_at"`
		RevocationReason     string        `json:"revocation_reason"`
	}{NodeID: r.NodeID, Kind: r.Kind, Message: r.Message, SessionID: r.SessionID, BootID: r.BootID, IP: r.IP,
		ExpectedVersion: r.ExpectedVersion, SupersedePending: r.SupersedePending, HoldDispatch: r.HoldDispatch, TTLSeconds: int64(r.TTL / time.Second),
		ActorID: actorID, Action: action, Reason: reason, ActorSessionID: r.ActorSessionID, ActorIdentityID: r.ActorIdentityID,
		ApprovalID: r.ApprovalID, CandidateHash: fmt.Sprintf("%x", r.CandidateHash), ExpectedCurrentHash: fmt.Sprintf("%x", r.ExpectedCurrentHash),
		DesiredRevision: r.DesiredRevision, PlanID: applyPlanID(r.ApplyMetadata), PlanRevision: r.PlanRevision,
		PlanTemplate: planTemplate(r.PlanMetadata), OcservVersion: r.OcservVersion, PlanCapabilities: r.PlanCapabilities,
		CertificateID: idempotencyCertificateID(r), CommonName: r.CommonName, DNSNames: r.DNSNames, KeyBits: r.KeyBits, ArtifactID: r.ArtifactID,
		CertificateChainHash: hashBytes(r.CertificateChain), SealedPasswordHash: hashBytes(r.SealedPassword), SecretKeyID: r.SecretKeyID,
		ArtifactTokenHash: artifactTokenHash(r.ArtifactMetadata), ArtifactRequestHash: artifactRequestHash(r.ArtifactMetadata), ArtifactExpiresAt: artifactExpiry(r.ArtifactMetadata),
		RevocationReason: r.RevocationReason}
	encoded, err := json.Marshal(intent)
	if err != nil {
		panic("marshal fixed idempotency intent: " + err.Error())
	}
	return sha256.Sum256(encoded)
}

func idempotencyCertificateID(request CreateRequest) uuid.UUID {
	if request.Kind == CertificateCSR {
		return uuid.Nil
	}
	return request.CertificateID
}

func hashBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func artifactTokenHash(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return hex.EncodeToString(metadata.TokenSHA256)
}

func artifactRequestHash(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return hex.EncodeToString(metadata.RequestHash)
}

func artifactExpiry(metadata *ArtifactMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.ExpiresAt.UTC().Format(time.RFC3339Nano)
}

func planTemplate(metadata *ConfigPlanMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.TemplateName
}

func applyPlanID(metadata *ConfigApplyMetadata) uuid.UUID {
	if metadata == nil {
		return uuid.Nil
	}
	return metadata.PlanID
}

func normalizedAuditIntent(r CreateRequest) (actorID, action, reason string) {
	actorID, action, reason = r.ActorID, r.Action, r.Reason
	if actorID == "" {
		actorID = "developer"
	}
	if action == "" {
		action = "operation.create"
	}
	if reason == "" {
		reason = "side-effect-free delivery validation"
	}
	return actorID, action, reason
}

func marshalEnvelope(r CreateRequest, operationID, commandID uuid.UUID, authorizationRevision uint64, now, expires time.Time, signer *commandauth.Signer) ([]byte, string, error) {
	messageID, err := uuid.NewV7()
	if err != nil {
		return nil, "", err
	}
	actorID, action, reason := normalizedAuditIntent(r)
	envelope := &agentv1.CommandEnvelope{ProtocolVersion: commandauth.ProtocolVersion, MessageId: messageID[:], CommandId: commandID[:], IdempotencyKey: operationID[:], NodeId: r.NodeID[:], Sequence: 1, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), ExpectedRevision: authorizationRevision, Traceparent: r.Traceparent, ActorId: actorID, Reason: reason, OperationId: operationID[:], Action: action, DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY}
	if r.ApprovalID != uuid.Nil {
		envelope.ApprovalId = r.ApprovalID[:]
	}
	if len(r.ApprovalRequestHash) != 0 {
		envelope.ApprovalRequestSha256 = append([]byte(nil), r.ApprovalRequestHash...)
	}
	payloadType := "synthetic_noop"
	switch r.Kind {
	case SyntheticEcho:
		payloadType = "synthetic_echo"
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: r.Message}}
	case SyntheticNoop:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case SessionDisconnect:
		payloadType = "session_disconnect"
		envelope.Payload = &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: r.SessionID, BootId: r.BootID}}
	case SessionTerminate:
		payloadType = "session_terminate"
		envelope.Payload = &agentv1.CommandEnvelope_SessionTerminate{SessionTerminate: &agentv1.SessionTerminate{SessionId: r.SessionID, BootId: r.BootID}}
	case IPBanRemove:
		payloadType = "ip_ban_remove"
		envelope.Payload = &agentv1.CommandEnvelope_IpBanRemove{IpBanRemove: &agentv1.IpBanRemove{Ip: r.IP}}
	case ServiceReload:
		payloadType = "service_reload"
		envelope.Payload = &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}
	case ConfigPlan:
		payloadType = "config_plan"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigPlan{ConfigPlan: &agentv1.ConfigPlan{Candidate: r.Candidate, CandidateHash: r.CandidateHash, ExpectedRevision: r.PlanRevision}}
	case ConfigApply:
		payloadType = "config_apply"
		envelope.Payload = &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{Candidate: r.Candidate, CandidateHash: r.CandidateHash, ExpectedCurrentHash: r.ExpectedCurrentHash, DesiredRevision: r.DesiredRevision}}
	case CertificateCSR:
		payloadType = "certificate_csr"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: r.CertificateID[:], CommonName: r.CommonName, DnsNames: r.DNSNames, KeyBits: r.KeyBits}}
	case CertificateP12:
		payloadType = "certificate_p12"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateP12{CertificateP12: &agentv1.CertificateP12{CertificateId: r.CertificateID[:], CertificateChainPem: r.CertificateChain, SealedPassword: r.SealedPassword, SecretKeyId: r.SecretKeyID, ArtifactId: r.ArtifactID[:]}}
	case CertificateRevoke:
		payloadType = "certificate_revoke"
		envelope.Payload = &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{CertificateId: r.CertificateID[:], Reason: r.RevocationReason}}
	}
	if err := semanticpayload.PopulateV2(envelope); err != nil {
		return nil, "", fmt.Errorf("compute semantic payload hash: %w", err)
	}
	envelope.RequiredCapability = capabilityFor(r.Kind)
	if envelope.RequiredCapability == "" {
		switch r.Kind {
		case SyntheticNoop:
			envelope.RequiredCapability = "synthetic.noop"
		case SyntheticEcho:
			envelope.RequiredCapability = "synthetic.echo"
		}
	}
	if err := signer.Authorize(envelope); err != nil {
		return nil, "", fmt.Errorf("authorize typed command: %w", err)
	}
	data, err := proto.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("marshal typed command: %w", err)
	}
	return data, payloadType, nil
}

func capabilityFor(kind SyntheticKind) string {
	switch kind {
	case SessionDisconnect:
		return "ocserv.session.disconnect"
	case SessionTerminate:
		return "ocserv.session.terminate"
	case IPBanRemove:
		return "ocserv.ip_ban.remove"
	case ServiceReload:
		return "ocserv.service.reload"
	case ConfigPlan:
		return "ocserv.config.plan"
	case ConfigApply:
		return "ocserv.config.apply"
	case CertificateCSR, CertificateP12:
		return "ocserv.certificate.issue"
	case CertificateRevoke:
		return "ocserv.certificate.revoke"
	default:
		return ""
	}
}

func validDNSName(value string) bool {
	if len(value) < 1 || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func findIdempotent(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, key string, hash []byte) (Operation, bool, error) {
	row := tx.QueryRow(ctx, `SELECT o.id::text,o.state,o.node_id::text,o.command_id::text,o.version,o.created_at,o.updated_at,o.expires_at,o.request_hash=$3,COALESCE(x.state,''),COALESCE(x.failure_code,'') FROM operations o LEFT JOIN config_apply_operations x ON x.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspaceID, key, hash)
	var op Operation
	var nodeID, commandID *string
	var same bool
	err := row.Scan(&op.ID, &op.State, &nodeID, &commandID, &op.Version, &op.CreatedAt, &op.UpdatedAt, &op.ExpiresAt, &same, &op.ConfigApplyState, &op.ConfigApplyFailureCode)
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
	err := row.Scan(&op.ID, &op.State, &nodeID, &commandID, &op.Version, &op.CreatedAt, &op.UpdatedAt, &op.ExpiresAt, &op.ConfigApplyState, &op.ConfigApplyFailureCode)
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

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
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
