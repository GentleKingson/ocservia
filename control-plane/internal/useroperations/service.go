// Package useroperations manages quota, expiry, batch operations, and their scheduler.
package useroperations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultGlobalConcurrency = 50
	MaxBatchItems            = 500
	MaxBatchRefresh          = 100
	MaxSafeQuotaBytes        = 1<<53 - 1
	leaseName                = "user-operations"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var (
	ErrInvalidRequest      = errors.New("user operations request is invalid")
	ErrNotFound            = errors.New("user policy was not found")
	ErrVersionConflict     = errors.New("user policy version is stale")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")
)

type Policy struct {
	NodeID          uuid.UUID  `json:"node_id"`
	Username        string     `json:"username"`
	QuotaPeriod     string     `json:"quota_period"`
	QuotaDirection  string     `json:"quota_direction"`
	QuotaBytes      int64      `json:"quota_bytes"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Version         int64      `json:"version"`
	PeriodStart     time.Time  `json:"period_start"`
	ObservedRXBytes int64      `json:"observed_rx_bytes"`
	ObservedTXBytes int64      `json:"observed_tx_bytes"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	Exceeded        bool       `json:"exceeded"`
	Expired         bool       `json:"expired"`
	Convergence     string     `json:"convergence"`
}

type PolicyRequest struct {
	NodeID, ActorIdentityID, ActorSessionID uuid.UUID
	Username, QuotaPeriod, QuotaDirection   string
	QuotaBytes, ExpectedVersion             int64
	ExpiresAt                               *time.Time
	IdempotencyKey, ActorID, Reason         string
	RequestID, Traceparent                  string
}

type BatchItemRequest struct {
	NodeID          uuid.UUID `json:"node_id"`
	Username        string    `json:"username"`
	Action          string    `json:"action"`
	ExpectedVersion int64     `json:"expected_version"`
	Authorized      bool      `json:"-"`
}

type BatchRequest struct {
	ID, WorkspaceID, ActorIdentityID, ActorSessionID, ApprovalID uuid.UUID
	ActorID, Reason, RequestID, Traceparent                      string
	IdempotencyKey                                               string
	Items                                                        []BatchItemRequest
}

type BatchItem struct {
	Index            int        `json:"index"`
	NodeID           uuid.UUID  `json:"node_id"`
	Username         string     `json:"username"`
	Action           string     `json:"action"`
	ExpectedVersion  int64      `json:"expected_version"`
	State            string     `json:"state"`
	ChildOperationID *uuid.UUID `json:"child_operation_id,omitempty"`
	ErrorType        string     `json:"error_type,omitempty"`
}

type Batch struct {
	ID              uuid.UUID   `json:"id"`
	WorkspaceID     uuid.UUID   `json:"workspace_id"`
	ActorIdentityID *uuid.UUID  `json:"-"`
	State           string      `json:"state"`
	Items           []BatchItem `json:"items"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

type UsageSample = userusage.Sample

type Metrics struct {
	PolicyPendingTotal    int64 `json:"policy_pending_total"`
	ActiveBatchItemTotal  int64 `json:"active_batch_item_total"`
	StaleBatchClaimTotal  int64 `json:"stale_batch_claim_total"`
	UnknownBatchItemTotal int64 `json:"unknown_batch_item_total"`
}

type Service struct {
	pool      *pgxpool.Pool
	users     *userstate.Service
	now       func() time.Time
	newID     func() uuid.UUID
	batchSize int
}

func New(pool *pgxpool.Pool, users *userstate.Service) *Service {
	return &Service{pool: pool, users: users, now: func() time.Time { return time.Now().UTC() }, newID: func() uuid.UUID { return uuid.Must(uuid.NewV7()) }, batchSize: DefaultGlobalConcurrency}
}

func NewWithConcurrency(pool *pgxpool.Pool, users *userstate.Service, concurrency int) *Service {
	service := New(pool, users)
	if concurrency > 0 {
		service.batchSize = concurrency
	}
	return service
}

func (s *Service) SetPolicy(ctx context.Context, request PolicyRequest) (Policy, bool, error) {
	if err := validatePolicyRequest(request); err != nil {
		return Policy{}, false, err
	}
	request.Username = strings.TrimSpace(request.Username)
	hash := policyHash(request)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Policy{}, false, err
	}
	defer rollback(tx)
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT n.workspace_id FROM desired_users u JOIN nodes n ON n.id=u.node_id WHERE u.node_id=$1 AND u.username=$2 AND n.status IN('active','offline') FOR UPDATE OF u`, request.NodeID, request.Username).Scan(&workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Policy{}, false, ErrNotFound
		}
		return Policy{}, false, err
	}
	var replayVersion int64
	var replayHash []byte
	err = tx.QueryRow(ctx, `SELECT policy_version,request_hash FROM user_policy_mutations WHERE workspace_id=$1 AND idempotency_key=$2`, workspaceID, request.IdempotencyKey).Scan(&replayVersion, &replayHash)
	if err == nil {
		if !slices.Equal(replayHash, hash[:]) {
			return Policy{}, false, ErrIdempotencyConflict
		}
		policy, readErr := readPolicy(ctx, tx, request.NodeID, request.Username, s.now())
		if readErr != nil {
			return Policy{}, false, readErr
		}
		if policy.Version < replayVersion {
			return Policy{}, false, errors.New("replayed policy version is unavailable")
		}
		return policy, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, false, err
	}
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT version FROM desired_user_policies WHERE node_id=$1 AND username=$2 FOR UPDATE`, request.NodeID, request.Username).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		currentVersion = 0
	} else if err != nil {
		return Policy{}, false, err
	}
	if currentVersion != request.ExpectedVersion {
		return Policy{}, false, ErrVersionConflict
	}
	now := s.now()
	nextVersion := currentVersion + 1
	_, err = tx.Exec(ctx, `INSERT INTO desired_user_policies(node_id,username,quota_period,quota_direction,quota_bytes,expires_at,version,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8)
		ON CONFLICT(node_id,username) DO UPDATE SET quota_period=EXCLUDED.quota_period,quota_direction=EXCLUDED.quota_direction,quota_bytes=EXCLUDED.quota_bytes,expires_at=EXCLUDED.expires_at,version=EXCLUDED.version,updated_at=EXCLUDED.updated_at`,
		request.NodeID, request.Username, request.QuotaPeriod, request.QuotaDirection, request.QuotaBytes, request.ExpiresAt, nextVersion, now)
	if err != nil {
		return Policy{}, false, err
	}
	mutationID := s.newID()
	if _, err := tx.Exec(ctx, `INSERT INTO user_policy_mutations(id,workspace_id,node_id,username,idempotency_key,request_hash,policy_version,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, mutationID, workspaceID, request.NodeID, request.Username, request.IdempotencyKey, hash[:], nextVersion, now); err != nil {
		return Policy{}, false, err
	}
	after, _ := json.Marshal(map[string]any{"quota_period": request.QuotaPeriod, "quota_direction": request.QuotaDirection, "quota_bytes": request.QuotaBytes, "expires_at": request.ExpiresAt, "version": nextVersion})
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: "user.policy.set", ResourceType: "user_policy", ResourceID: mutationID, NodeID: &request.NodeID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, AfterSummary: after, At: now}); err != nil {
		return Policy{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Policy{}, false, err
	}
	policy, err := s.GetPolicy(ctx, request.NodeID, request.Username)
	return policy, false, err
}

func (s *Service) GetPolicy(ctx context.Context, nodeID uuid.UUID, username string) (Policy, error) {
	return readPolicy(ctx, s.pool, nodeID, username, s.now())
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readPolicy(ctx context.Context, q queryer, nodeID uuid.UUID, username string, now time.Time) (Policy, error) {
	policy := Policy{NodeID: nodeID, Username: username}
	month := monthStart(now)
	var nodeStatus string
	var desiredEnabled bool
	var desiredRevision int64
	var observedEnabled *bool
	var observedRevision *int64
	var operationState *string
	err := q.QueryRow(ctx, `SELECT p.quota_period,p.quota_direction,p.quota_bytes,p.expires_at,p.version,
		CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,
		COALESCE(u.rx_bytes,0),COALESCE(u.tx_bytes,0),u.observed_at,
		n.status,d.enabled,d.revision,o.enabled,o.revision,latest.state
		FROM desired_user_policies p JOIN nodes n ON n.id=p.node_id JOIN desired_users d ON d.node_id=p.node_id AND d.username=p.username
		LEFT JOIN observed_users o ON o.node_id=p.node_id AND o.username=p.username
		LEFT JOIN observed_user_usage u ON u.node_id=p.node_id AND u.username=p.username AND u.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND u.period_start=CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
		LEFT JOIN LATERAL (SELECT op.state FROM commands command JOIN operations op ON op.id=command.operation_id WHERE command.node_id=p.node_id AND command.resource_type='user' AND command.resource_key=p.username ORDER BY command.created_at DESC,command.id DESC LIMIT 1) latest ON true
		WHERE p.node_id=$1 AND p.username=$2`, nodeID, username, month).Scan(&policy.QuotaPeriod, &policy.QuotaDirection, &policy.QuotaBytes, &policy.ExpiresAt, &policy.Version, &policy.PeriodStart, &policy.ObservedRXBytes, &policy.ObservedTXBytes, &policy.ObservedAt, &nodeStatus, &desiredEnabled, &desiredRevision, &observedEnabled, &observedRevision, &operationState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, err
	}
	policy.Exceeded = quotaValue(policy.QuotaDirection, policy.ObservedRXBytes, policy.ObservedTXBytes) >= policy.QuotaBytes && policy.QuotaPeriod != "none"
	policy.Expired = policy.ExpiresAt != nil && !policy.ExpiresAt.After(now)
	triggerPending := (policy.Exceeded || policy.Expired) && desiredEnabled
	observedMatches := observedEnabled != nil && observedRevision != nil && *observedEnabled == desiredEnabled && *observedRevision == desiredRevision
	switch {
	case observedMatches && !triggerPending:
		policy.Convergence = "converged"
	case nodeStatus == "offline":
		policy.Convergence = "offline_pending"
	case operationState != nil && slices.Contains([]string{"queued", "dispatched", "accepted", "running", "offline_pending"}, *operationState):
		policy.Convergence = "pending"
	case triggerPending:
		policy.Convergence = "pending"
	default:
		policy.Convergence = "drifted"
	}
	return policy, nil
}

func (s *Service) CreateBatch(ctx context.Context, request BatchRequest) (Batch, bool, error) {
	if err := validateBatchRequest(request); err != nil {
		return Batch{}, false, err
	}
	hash := BatchRequestHash(request.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Batch{}, false, err
	}
	defer rollback(tx)
	var existing uuid.UUID
	var existingHash []byte
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM batch_operations WHERE workspace_id=$1 AND idempotency_key=$2`, request.WorkspaceID, request.IdempotencyKey).Scan(&existing, &existingHash)
	if err == nil {
		if existing != request.ID || !slices.Equal(existingHash, hash[:]) {
			return Batch{}, false, ErrIdempotencyConflict
		}
		if hasDisable(request.Items) {
			if err := approvals.ValidateConsumedBound(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "user.batch.disable", "batch_operation", existing, hash[:]); err != nil {
				return Batch{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Batch{}, false, err
		}
		batch, getErr := s.GetBatch(ctx, existing)
		return batch, true, getErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, false, err
	}
	id, now := request.ID, s.now()
	if hasDisable(request.Items) {
		if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "user.batch.disable", "batch_operation", id, hash[:]); err != nil {
			return Batch{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO batch_operations(id,workspace_id,state,actor_identity_id,actor_session_id,approval_id,actor_id,reason,request_id,traceparent,idempotency_key,request_hash,created_at,updated_at) VALUES($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`, id, request.WorkspaceID, optionalUUID(request.ActorIdentityID), optionalUUID(request.ActorSessionID), optionalUUID(request.ApprovalID), request.ActorID, request.Reason, request.RequestID, request.Traceparent, request.IdempotencyKey, hash[:], now); err != nil {
		return Batch{}, false, err
	}
	for index, item := range request.Items {
		state, errorType := "queued", ""
		if !item.Authorized {
			state, errorType = "forbidden", "forbidden"
		} else {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM desired_users u JOIN nodes n ON n.id=u.node_id WHERE u.node_id=$1 AND u.username=$2 AND n.workspace_id=$3)`, item.NodeID, item.Username, request.WorkspaceID).Scan(&exists); err != nil {
				return Batch{}, false, err
			}
			if !exists {
				state, errorType = "failed", "not_found"
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO batch_operation_items(batch_id,item_index,node_id,username,action,expected_version,state,error_type,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`, id, index, item.NodeID, item.Username, item.Action, item.ExpectedVersion, state, errorType, now); err != nil {
			return Batch{}, false, err
		}
	}
	auditSummary, _ := json.Marshal(map[string]any{"request_hash": hex.EncodeToString(hash[:]), "items": request.Items})
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: "user.batch.create", ResourceType: "batch_operation", ResourceID: id, ApprovalID: optionalUUID(request.ApprovalID), RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, AfterSummary: auditSummary, At: now}); err != nil {
		return Batch{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Batch{}, false, err
	}
	batch, err := s.GetBatch(ctx, id)
	return batch, false, err
}

func (s *Service) GetBatch(ctx context.Context, id uuid.UUID) (Batch, error) {
	var batch Batch
	batch.ID = id
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id,actor_identity_id,state,created_at,updated_at FROM batch_operations WHERE id=$1`, id).Scan(&batch.WorkspaceID, &batch.ActorIdentityID, &batch.State, &batch.CreatedAt, &batch.UpdatedAt); err != nil {
		return Batch{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT item_index,node_id,username,action,expected_version,state,child_operation_id,COALESCE(error_type,'') FROM batch_operation_items WHERE batch_id=$1 ORDER BY item_index`, id)
	if err != nil {
		return Batch{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item BatchItem
		if err := rows.Scan(&item.Index, &item.NodeID, &item.Username, &item.Action, &item.ExpectedVersion, &item.State, &item.ChildOperationID, &item.ErrorType); err != nil {
			return Batch{}, err
		}
		batch.Items = append(batch.Items, item)
	}
	return batch, rows.Err()
}

func (s *Service) Metrics(ctx context.Context, workspaceID uuid.UUID) (Metrics, error) {
	var value Metrics
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM desired_user_policies policy JOIN nodes node ON node.id=policy.node_id JOIN desired_users desired USING(node_id,username) LEFT JOIN observed_users observed USING(node_id,username) LEFT JOIN observed_user_usage usage ON usage.node_id=policy.node_id AND usage.username=policy.username AND usage.period=CASE WHEN policy.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND usage.period_start=CASE WHEN policy.quota_period='monthly' THEN date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' ELSE '1970-01-01T00:00:00Z'::timestamptz END WHERE node.workspace_id=$1 AND ((desired.enabled=true AND ((policy.expires_at IS NOT NULL AND policy.expires_at<=now()) OR (policy.quota_period<>'none' AND CASE policy.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END>=policy.quota_bytes::numeric))) OR (EXISTS(SELECT 1 FROM user_policy_enforcements enforcement WHERE enforcement.node_id=policy.node_id AND enforcement.username=policy.username AND enforcement.policy_version=policy.version AND enforcement.operation_id IS NOT NULL) AND (observed.username IS NULL OR observed.enabled<>desired.enabled OR observed.revision<>desired.revision)))),
		(SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state IN('queued','submitting','submitted','offline_pending','unknown')),
		(SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state='submitting' AND item.lease_until<=now()),
		(SELECT count(*) FROM batch_operation_items item JOIN batch_operations batch ON batch.id=item.batch_id WHERE batch.workspace_id=$1 AND item.state='unknown')`, workspaceID).Scan(&value.PolicyPendingTotal, &value.ActiveBatchItemTotal, &value.StaleBatchClaimTotal, &value.UnknownBatchItemTotal)
	return value, err
}

// RunOnce obtains the database lease, compensates missed expiry/quota scans,
// submits at most the configured global command limit, and refreshes children.
func (s *Service) RunOnce(ctx context.Context) error {
	owner := s.newID()
	acquired, err := s.acquireLease(ctx, owner, 25*time.Second)
	if err != nil || !acquired {
		return err
	}
	if err := s.refreshBatches(ctx); err != nil {
		return err
	}
	active, err := s.activeUserOperationCount(ctx)
	if err != nil {
		return err
	}
	remaining := max(0, s.batchSize-active)
	used, err := s.resetMonthlyPolicies(ctx, remaining)
	if err != nil {
		return err
	}
	remaining -= used
	if remaining > 0 {
		used, err = s.enforcePolicies(ctx, remaining)
		if err != nil {
			return err
		}
		remaining -= used
	}
	if remaining > 0 {
		if err := s.submitBatchItems(ctx, owner, remaining); err != nil {
			return err
		}
	}
	return s.refreshBatches(ctx)
}

func (s *Service) activeUserOperationCount(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown')`).Scan(&count)
	return count, err
}

func (s *Service) resetMonthlyPolicies(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	now, month := s.now(), monthStart(s.now())
	rows, err := s.pool.Query(ctx, `SELECT p.node_id,p.username,p.version,u.version,u.enabled,prior.resulting_user_version
		FROM desired_user_policies p JOIN desired_users u USING(node_id,username)
		LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period='monthly' AND usage.period_start=$1
		JOIN LATERAL (SELECT resulting_user_version FROM user_policy_enforcements prior WHERE prior.node_id=p.node_id AND prior.username=p.username AND prior.policy_version=p.version AND prior.cause='quota' AND prior.period_start<$1 AND prior.operation_id IS NOT NULL ORDER BY prior.period_start DESC LIMIT 1) prior ON true
		WHERE p.quota_period='monthly' AND (p.expires_at IS NULL OR p.expires_at>$2)
		AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END<p.quota_bytes::numeric
		AND ((u.enabled=false AND prior.resulting_user_version=u.version) OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause='quota_reset' AND pending.period_start=$1 AND pending.source_user_version=u.version AND pending.operation_id IS NULL))
		AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements reset WHERE reset.node_id=p.node_id AND reset.username=p.username AND reset.policy_version=p.version AND reset.cause='quota_reset' AND reset.period_start=$1 AND reset.operation_id IS NOT NULL)
		ORDER BY p.node_id,p.username LIMIT $3`, month, now, limit)
	if err != nil {
		return 0, err
	}
	type resetCandidate struct {
		nodeID                                      uuid.UUID
		username                                    string
		policyVersion, userVersion, enforcedVersion int64
		enabled                                     bool
	}
	var candidates []resetCandidate
	for rows.Next() {
		var item resetCandidate
		if err := rows.Scan(&item.nodeID, &item.username, &item.policyVersion, &item.userVersion, &item.enabled, &item.enforcedVersion); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	processed := 0
	for _, item := range candidates {
		key := stableKey("policy-reset", item.nodeID.String(), item.username, fmt.Sprint(item.policyVersion), month.Format(time.RFC3339))
		if _, err := s.pool.Exec(ctx, `INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at) VALUES($1,$2,$3,'quota_reset',$4,$5,$6) ON CONFLICT DO NOTHING`, item.nodeID, item.username, item.policyVersion, month, item.userVersion, now); err != nil {
			return 0, err
		}
		if item.enabled {
			operationID, found, findErr := s.findUserOperation(ctx, item.nodeID, item.username, key, userstate.UserEnable)
			if findErr != nil {
				return 0, findErr
			}
			if !found {
				_, _ = s.pool.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause='quota_reset' AND period_start=$4 AND operation_id IS NULL`, item.nodeID, item.username, item.policyVersion, month)
				continue
			}
			if _, err := s.pool.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=$5,resulting_user_version=$6 WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause='quota_reset' AND period_start=$4 AND operation_id IS NULL`, item.nodeID, item.username, item.policyVersion, month, operationID, item.userVersion); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.nodeID, Kind: userstate.UserEnable, Name: item.username, ExpectedVersion: item.userVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "monthly quota reset", RequestID: key, Traceparent: stableTraceparent(key)})
		if mutateErr != nil {
			if errors.Is(mutateErr, userstate.ErrVersionConflict) || errors.Is(mutateErr, userstate.ErrRevisionPending) || errors.Is(mutateErr, userstate.ErrRevisionRecovery) {
				_, _ = s.pool.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause='quota_reset' AND period_start=$4 AND source_user_version=$5 AND operation_id IS NULL`, item.nodeID, item.username, item.policyVersion, month, item.userVersion)
				continue
			}
			return 0, mutateErr
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return 0, parseErr
		}
		if _, err := s.pool.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=$5,resulting_user_version=$6 WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause='quota_reset' AND period_start=$4 AND operation_id IS NULL`, item.nodeID, item.username, item.policyVersion, month, operationID, item.userVersion+1); err != nil {
			return 0, err
		}
		processed++
	}
	return processed, rows.Err()
}

func (s *Service) acquireLease(ctx context.Context, owner uuid.UUID, duration time.Duration) (bool, error) {
	result, err := s.pool.Exec(ctx, `INSERT INTO scheduler_leases(lease_name,owner_id,lease_until,updated_at) VALUES($1,$2,now()+$3::interval,now()) ON CONFLICT(lease_name) DO UPDATE SET owner_id=EXCLUDED.owner_id,lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at WHERE scheduler_leases.lease_until<=now() OR scheduler_leases.owner_id=EXCLUDED.owner_id`, leaseName, owner, duration.String())
	return err == nil && result.RowsAffected() == 1, err
}

type enforcementCandidate struct {
	nodeID      uuid.UUID
	username    string
	version     int64
	userVersion int64
	cause       string
	periodStart time.Time
	enabled     bool
}

func (s *Service) enforcePolicies(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	now, month := s.now(), monthStart(s.now())
	rows, err := s.pool.Query(ctx, `SELECT p.node_id,p.username,p.version,u.version,
		CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END,
		CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,u.enabled
		FROM desired_user_policies p JOIN desired_users u USING(node_id,username)
		LEFT JOIN observed_user_usage usage ON usage.node_id=p.node_id AND usage.username=p.username AND usage.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND usage.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
		WHERE ((p.expires_at IS NOT NULL AND p.expires_at<=$1) OR (p.quota_period<>'none' AND CASE p.quota_direction WHEN 'rx' THEN COALESCE(usage.rx_bytes,0)::numeric WHEN 'tx' THEN COALESCE(usage.tx_bytes,0)::numeric ELSE COALESCE(usage.rx_bytes,0)::numeric+COALESCE(usage.tx_bytes,0)::numeric END>=p.quota_bytes::numeric))
		AND (u.enabled=true OR EXISTS(SELECT 1 FROM user_policy_enforcements pending WHERE pending.node_id=p.node_id AND pending.username=p.username AND pending.policy_version=p.version AND pending.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND pending.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND pending.source_user_version=u.version AND pending.operation_id IS NULL))
		AND NOT EXISTS(SELECT 1 FROM user_policy_enforcements e WHERE e.node_id=p.node_id AND e.username=p.username AND e.policy_version=p.version AND e.cause=CASE WHEN p.expires_at IS NOT NULL AND p.expires_at<=$1 THEN 'expiry' ELSE 'quota' END AND e.period_start=CASE WHEN p.quota_period='monthly' THEN $2::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END AND e.operation_id IS NOT NULL)
		ORDER BY p.node_id,p.username LIMIT $3`, now, month, limit)
	if err != nil {
		return 0, err
	}
	var candidates []enforcementCandidate
	for rows.Next() {
		var item enforcementCandidate
		if err := rows.Scan(&item.nodeID, &item.username, &item.version, &item.userVersion, &item.cause, &item.periodStart, &item.enabled); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	processed := 0
	for _, item := range candidates {
		key := stableKey("policy", item.nodeID.String(), item.username, fmt.Sprint(item.version), item.cause, item.periodStart.Format(time.RFC3339))
		trace := stableTraceparent(key)
		if _, err := s.pool.Exec(ctx, `INSERT INTO user_policy_enforcements(node_id,username,policy_version,cause,period_start,source_user_version,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, item.nodeID, item.username, item.version, item.cause, item.periodStart, item.userVersion, now); err != nil {
			return 0, err
		}
		if !item.enabled {
			operationID, found, findErr := s.findUserOperation(ctx, item.nodeID, item.username, key, userstate.UserDisable)
			if findErr != nil {
				return 0, findErr
			}
			if !found {
				_, _ = s.pool.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND operation_id IS NULL`, item.nodeID, item.username, item.version, item.cause, item.periodStart)
				continue
			}
			if _, err := s.pool.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=$6,resulting_user_version=$7 WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND operation_id IS NULL`, item.nodeID, item.username, item.version, item.cause, item.periodStart, operationID, item.userVersion); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.nodeID, Kind: userstate.UserDisable, Name: item.username, ExpectedVersion: item.userVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: key, Traceparent: trace})
		if mutateErr != nil {
			if errors.Is(mutateErr, userstate.ErrVersionConflict) || errors.Is(mutateErr, userstate.ErrRevisionPending) || errors.Is(mutateErr, userstate.ErrRevisionRecovery) {
				_, _ = s.pool.Exec(ctx, `DELETE FROM user_policy_enforcements WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND source_user_version=$6 AND operation_id IS NULL`, item.nodeID, item.username, item.version, item.cause, item.periodStart, item.userVersion)
				continue
			}
			return 0, mutateErr
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return 0, parseErr
		}
		if _, err := s.pool.Exec(ctx, `UPDATE user_policy_enforcements SET operation_id=$6,resulting_user_version=$7 WHERE node_id=$1 AND username=$2 AND policy_version=$3 AND cause=$4 AND period_start=$5 AND operation_id IS NULL`, item.nodeID, item.username, item.version, item.cause, item.periodStart, operationID, item.userVersion+1); err != nil {
			return 0, err
		}
		processed++
	}
	return processed, nil
}

func (s *Service) findUserOperation(ctx context.Context, nodeID uuid.UUID, username, key string, kind userstate.MutationKind) (uuid.UUID, bool, error) {
	var operationID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT op.id FROM operations op JOIN commands command ON command.operation_id=op.id WHERE op.node_id=$1 AND op.idempotency_key=$2 AND command.resource_type='user' AND command.resource_key=$3 AND command.payload_type=$4`, nodeID, key, username, kind).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return operationID, err == nil, err
}

func (s *Service) submitBatchItems(ctx context.Context, owner uuid.UUID, limit int) error {
	rows, err := s.pool.Query(ctx, `WITH claim AS (SELECT batch_id,item_index FROM batch_operation_items WHERE (state='queued' AND lease_until IS NULL) OR (state='submitting' AND lease_until<=now()) ORDER BY updated_at,batch_id,item_index FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE batch_operation_items item SET state='submitting',lease_owner=$2,lease_until=now()+interval '20 seconds',updated_at=now() FROM claim WHERE item.batch_id=claim.batch_id AND item.item_index=claim.item_index RETURNING item.batch_id,item.item_index,item.node_id,item.username,item.action,item.expected_version`, limit, owner)
	if err != nil {
		return err
	}
	type claimed struct {
		batchID         uuid.UUID
		index           int
		nodeID          uuid.UUID
		username        string
		action          string
		expectedVersion int64
	}
	var items []claimed
	for rows.Next() {
		var item claimed
		if err := rows.Scan(&item.batchID, &item.index, &item.nodeID, &item.username, &item.action, &item.expectedVersion); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	rows.Close()
	for _, item := range items {
		var actorIdentity, actorSession *uuid.UUID
		var actorID, reason, requestID, traceparent string
		if err := s.pool.QueryRow(ctx, `SELECT actor_identity_id,actor_session_id,actor_id,reason,request_id,traceparent FROM batch_operations WHERE id=$1`, item.batchID).Scan(&actorIdentity, &actorSession, &actorID, &reason, &requestID, &traceparent); err != nil {
			return err
		}
		kind := userstate.UserDisable
		if item.action == "enable" {
			kind = userstate.UserEnable
		}
		key := stableKey("batch", item.batchID.String(), fmt.Sprint(item.index))
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.nodeID, Kind: kind, Name: item.username, ExpectedVersion: item.expectedVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: actorID, ActorIdentityID: derefUUID(actorIdentity), ActorSessionID: derefUUID(actorSession), Reason: reason, RequestID: requestID + ":" + fmt.Sprint(item.index), Traceparent: traceparent})
		if mutateErr != nil {
			_, err = s.pool.Exec(ctx, `UPDATE batch_operation_items SET state='failed',error_type=$4,lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE batch_id=$1 AND item_index=$2 AND lease_owner=$3`, item.batchID, item.index, owner, userstateErrorType(mutateErr))
			if err != nil {
				return err
			}
			continue
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return parseErr
		}
		if _, err := s.pool.Exec(ctx, `UPDATE batch_operation_items SET state='submitted',child_operation_id=$4,lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE batch_id=$1 AND item_index=$2 AND lease_owner=$3`, item.batchID, item.index, owner, operationID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) refreshBatches(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `WITH active AS (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT $1) UPDATE batch_operation_items item SET state=CASE WHEN op.state='queued' AND node.status='offline' THEN 'offline_pending' WHEN op.state='succeeded' THEN 'succeeded' WHEN op.state IN('failed','expired','rolled_back') THEN 'failed' WHEN op.state='unknown' THEN 'unknown' WHEN op.state='offline_pending' THEN 'offline_pending' ELSE item.state END,error_type=CASE WHEN op.state IN('failed','expired','rolled_back') THEN op.state ELSE item.error_type END,updated_at=now() FROM operations op JOIN nodes node ON node.id=op.node_id,active WHERE item.batch_id=active.id AND item.child_operation_id=op.id AND item.state IN('submitted','unknown','offline_pending')`, MaxBatchRefresh); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `WITH active AS (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT $1),summary AS (SELECT item.batch_id,CASE WHEN bool_or(item.state IN('queued','submitting','submitted','unknown','offline_pending')) THEN CASE WHEN bool_or(item.state IN('failed','forbidden')) THEN 'partial_failed' ELSE 'running' END WHEN bool_and(item.state='succeeded') THEN 'succeeded' WHEN bool_or(item.state='succeeded') THEN 'partial_failed' ELSE 'failed' END state FROM batch_operation_items item JOIN active ON active.id=item.batch_id GROUP BY item.batch_id) UPDATE batch_operations batch SET state=summary.state,updated_at=now() FROM summary WHERE batch.id=summary.batch_id`, MaxBatchRefresh)
	return err
}

// RecordUsageTx converts monotonically increasing per-session counters into
// durable monthly and lifetime UTC usage without double-counting replays.
func RecordUsageTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID, samples []UsageSample) error {
	err := userusage.RecordTx(ctx, tx, nodeID, samples)
	if errors.Is(err, userusage.ErrInvalidSample) {
		return ErrInvalidRequest
	}
	return err
}

func validatePolicyRequest(request PolicyRequest) error {
	if request.NodeID == uuid.Nil || !namePattern.MatchString(request.Username) || request.ExpectedVersion < 0 || request.QuotaBytes < 0 || request.QuotaBytes > MaxSafeQuotaBytes || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || request.RequestID == "" || !validTraceparent(request.Traceparent) {
		return ErrInvalidRequest
	}
	if !slices.Contains([]string{"none", "monthly", "lifetime"}, request.QuotaPeriod) || !slices.Contains([]string{"rx", "tx", "rxtx"}, request.QuotaDirection) || (request.QuotaPeriod == "none") != (request.QuotaBytes == 0) {
		return ErrInvalidRequest
	}
	if request.ExpiresAt != nil {
		_, offset := request.ExpiresAt.Zone()
		if offset != 0 || request.ExpiresAt.Nanosecond() != 0 {
			return ErrInvalidRequest
		}
	}
	return nil
}

func validateBatchRequest(request BatchRequest) error {
	if request.ID == uuid.Nil || request.ID.Version() != 7 || request.WorkspaceID == uuid.Nil || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 512 || request.RequestID == "" || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || !validTraceparent(request.Traceparent) || ValidateBatchItems(request.Items) != nil {
		return ErrInvalidRequest
	}
	if hasDisable(request.Items) && (request.ActorIdentityID == uuid.Nil || request.ApprovalID == uuid.Nil) {
		return approvals.ErrNotReady
	}
	return nil
}

func ValidateBatchItems(items []BatchItemRequest) error {
	if len(items) == 0 || len(items) > MaxBatchItems {
		return ErrInvalidRequest
	}
	for _, item := range items {
		if item.NodeID == uuid.Nil || !namePattern.MatchString(item.Username) || item.ExpectedVersion < 1 || !slices.Contains([]string{"disable", "enable"}, item.Action) {
			return ErrInvalidRequest
		}
	}
	return nil
}

func hasDisable(items []BatchItemRequest) bool {
	return slices.ContainsFunc(items, func(item BatchItemRequest) bool { return item.Action == "disable" })
}

func policyHash(request PolicyRequest) [32]byte {
	encoded, _ := json.Marshal(struct {
		NodeID                      uuid.UUID `json:"node_id"`
		Username, Period, Direction string
		Bytes, Version              int64
		Expires                     *time.Time
	}{request.NodeID, request.Username, request.QuotaPeriod, request.QuotaDirection, request.QuotaBytes, request.ExpectedVersion, request.ExpiresAt})
	return sha256.Sum256(encoded)
}

func BatchRequestHash(items []BatchItemRequest) [32]byte {
	encoded, _ := json.Marshal(items)
	return sha256.Sum256(encoded)
}

func quotaValue(direction string, rx, tx int64) int64 {
	switch direction {
	case "rx":
		return rx
	case "tx":
		return tx
	default:
		if rx > 0 && tx > 0 && rx > (1<<63-1)-tx {
			return 1<<63 - 1
		}
		return rx + tx
	}
}

func monthStart(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func stableKey(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "i14-" + hex.EncodeToString(digest[:])
}

func stableTraceparent(seed string) string {
	trace := sha256.Sum256([]byte("trace:" + seed))
	span := sha256.Sum256([]byte("span:" + seed))
	return "00-" + hex.EncodeToString(trace[:16]) + "-" + hex.EncodeToString(span[:8]) + "-01"
}

func userstateErrorType(err error) string {
	switch {
	case errors.Is(err, userstate.ErrVersionConflict):
		return "stale_revision"
	case errors.Is(err, userstate.ErrRevisionPending):
		return "revision_pending"
	case errors.Is(err, userstate.ErrRevisionRecovery):
		return "recovery_required"
	case errors.Is(err, userstate.ErrCapabilityMissing):
		return "capability_unavailable"
	case errors.Is(err, userstate.ErrNodeUnavailable):
		return "node_unavailable"
	default:
		return "submission_failed"
	}
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func derefUUID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func traceID(value string) string { return value[3:35] }

func validTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	for _, part := range parts[1:] {
		for _, c := range part {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return parts[1] != strings.Repeat("0", 32) && parts[2] != strings.Repeat("0", 16)
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
