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
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/GentleKingson/ocservia/control-plane/internal/userusage"
	"github.com/google/uuid"
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
	NodeID          uuid.UUID        `json:"node_id"`
	Username        string           `json:"username"`
	QuotaPeriod     string           `json:"quota_period"`
	QuotaDirection  string           `json:"quota_direction"`
	QuotaBytes      int64            `json:"quota_bytes"`
	ExpiresAt       *value.Timestamp `json:"expires_at,omitempty"`
	Version         int64            `json:"version"`
	PeriodStart     value.Timestamp  `json:"period_start"`
	ObservedRXBytes int64            `json:"observed_rx_bytes"`
	ObservedTXBytes int64            `json:"observed_tx_bytes"`
	ObservedAt      *value.Timestamp `json:"observed_at,omitempty"`
	Exceeded        bool             `json:"exceeded"`
	Expired         bool             `json:"expired"`
	Convergence     string           `json:"convergence"`
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
	ID              uuid.UUID       `json:"id"`
	WorkspaceID     uuid.UUID       `json:"workspace_id"`
	ActorIdentityID *uuid.UUID      `json:"-"`
	State           string          `json:"state"`
	Items           []BatchItem     `json:"items"`
	CreatedAt       value.Timestamp `json:"created_at"`
	UpdatedAt       value.Timestamp `json:"updated_at"`
}

type UsageSample = userusage.Sample

type Metrics = userstore.Metrics

type Service struct {
	backend   database.Backend
	users     *userstate.Service
	now       func() time.Time
	newID     func() uuid.UUID
	batchSize int
}

func New(pool *pgxpool.Pool, users *userstate.Service) *Service {
	return NewBackend(postgres.WrapPool(pool), users)
}

func NewBackend(backend database.Backend, users *userstate.Service) *Service {
	return &Service{backend: backend, users: users, now: func() time.Time { return time.Now().UTC() }, newID: func() uuid.UUID { return uuid.Must(uuid.NewV7()) }, batchSize: DefaultGlobalConcurrency}
}

func NewWithConcurrency(pool *pgxpool.Pool, users *userstate.Service, concurrency int) *Service {
	return NewWithConcurrencyBackend(postgres.WrapPool(pool), users, concurrency)
}

func NewWithConcurrencyBackend(backend database.Backend, users *userstate.Service, concurrency int) *Service {
	service := NewBackend(backend, users)
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
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return Policy{}, false, err
	}
	defer rollback(tx)
	store, err := userstore.From(tx)
	if err != nil {
		return Policy{}, false, err
	}
	workspaceID, err := store.LockUser(ctx, request.NodeID, request.Username)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return Policy{}, false, ErrNotFound
		}
		return Policy{}, false, err
	}
	receipt, err := store.Mutation(ctx, workspaceID, request.IdempotencyKey)
	if err == nil {
		if !slices.Equal(receipt.Hash, hash[:]) {
			return Policy{}, false, ErrIdempotencyConflict
		}
		policy, readErr := readPolicy(ctx, store, request.NodeID, request.Username, s.now())
		if readErr != nil {
			return Policy{}, false, readErr
		}
		if policy.Version < receipt.Version {
			return Policy{}, false, errors.New("replayed policy version is unavailable")
		}
		return policy, true, commit(ctx, tx)
	}
	if !errors.Is(err, database.ErrNotFound) {
		return Policy{}, false, err
	}
	currentVersion, err := store.LockPolicy(ctx, request.NodeID, request.Username)
	if errors.Is(err, database.ErrNotFound) {
		currentVersion = 0
	} else if err != nil {
		return Policy{}, false, err
	}
	if currentVersion != request.ExpectedVersion {
		return Policy{}, false, ErrVersionConflict
	}
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return Policy{}, false, err
	}
	var expires value.Timestamp
	if request.ExpiresAt != nil {
		expires, err = value.FromTime(*request.ExpiresAt)
		if err != nil {
			return Policy{}, false, ErrInvalidRequest
		}
	}
	nextVersion := currentVersion + 1
	err = store.PutPolicy(ctx, userstore.Policy{NodeID: request.NodeID, Username: request.Username, QuotaPeriod: request.QuotaPeriod, QuotaDirection: request.QuotaDirection, QuotaBytes: request.QuotaBytes, ExpiresAt: expires, Version: nextVersion, CreatedAt: at, UpdatedAt: at})
	if err != nil {
		return Policy{}, false, err
	}
	mutationID := s.newID()
	if err := store.InsertMutation(ctx, userstore.Mutation{ID: mutationID, WorkspaceID: workspaceID, NodeID: request.NodeID, Username: request.Username, IdempotencyKey: request.IdempotencyKey, Hash: hash[:], Version: nextVersion, At: at}); err != nil {
		return Policy{}, false, err
	}
	after, _ := json.Marshal(map[string]any{"quota_period": request.QuotaPeriod, "quota_direction": request.QuotaDirection, "quota_bytes": request.QuotaBytes, "expires_at": request.ExpiresAt, "version": nextVersion})
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: "user.policy.set", ResourceType: "user_policy", ResourceID: mutationID, NodeID: &request.NodeID, RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, AfterSummary: after, At: now}); err != nil {
		return Policy{}, false, err
	}
	if err := commit(ctx, tx); err != nil {
		return Policy{}, false, err
	}
	policy, err := s.GetPolicy(ctx, request.NodeID, request.Username)
	return policy, false, err
}

func (s *Service) GetPolicy(ctx context.Context, nodeID uuid.UUID, username string) (Policy, error) {
	var policy Policy
	err := s.withStore(ctx, func(store userstore.Store) (err error) {
		policy, err = readPolicy(ctx, store, nodeID, username, s.now())
		return
	})
	return policy, err
}

func readPolicy(ctx context.Context, store userstore.Store, nodeID uuid.UUID, username string, now time.Time) (Policy, error) {
	month, err := value.FromTime(monthStart(now))
	if err != nil {
		return Policy{}, err
	}
	at, err := value.FromTime(now)
	if err != nil {
		return Policy{}, err
	}
	v, err := store.Policy(ctx, nodeID, username, month)
	if errors.Is(err, database.ErrNotFound) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, err
	}
	policy := Policy{NodeID: nodeID, Username: username, QuotaPeriod: v.QuotaPeriod, QuotaDirection: v.QuotaDirection, QuotaBytes: v.QuotaBytes, Version: v.Version, PeriodStart: v.PeriodStart, ObservedRXBytes: v.ObservedRXBytes, ObservedTXBytes: v.ObservedTXBytes}
	if v.ExpiresAt.Valid {
		policy.ExpiresAt = &v.ExpiresAt
	}
	if v.ObservedAt.Valid {
		policy.ObservedAt = &v.ObservedAt
	}
	policy.Exceeded = quotaValue(policy.QuotaDirection, policy.ObservedRXBytes, policy.ObservedTXBytes) >= policy.QuotaBytes && policy.QuotaPeriod != "none"
	policy.Expired = v.ExpiresAt.Valid && v.ExpiresAt.Micros <= at.Micros
	triggerPending := (policy.Exceeded || policy.Expired) && v.DesiredEnabled
	observedMatches := v.ObservedEnabled != nil && v.ObservedRevision != nil && *v.ObservedEnabled == v.DesiredEnabled && *v.ObservedRevision == v.DesiredRevision
	switch {
	case observedMatches && !triggerPending:
		policy.Convergence = "converged"
	case v.NodeStatus == "offline":
		policy.Convergence = "offline_pending"
	case v.OperationState != nil && slices.Contains([]string{"queued", "dispatched", "accepted", "running", "offline_pending"}, *v.OperationState):
		policy.Convergence = "pending"
	case triggerPending:
		policy.Convergence = "pending"
	default:
		policy.Convergence = "drifted"
	}
	return policy, nil
}

func (s *Service) CreateBatch(ctx context.Context, request BatchRequest) (Batch, bool, error) {
	if request.ID == uuid.Nil {
		request.ID = s.newID()
	}
	if err := validateBatchRequest(request); err != nil {
		return Batch{}, false, err
	}
	hash := BatchRequestHash(request.Items)
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return Batch{}, false, err
	}
	defer rollback(tx)
	store, err := userstore.From(tx)
	if err != nil {
		return Batch{}, false, err
	}
	if hasDisable(request.Items) {
		approvedID, approvedHash, err := store.BatchApproval(ctx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID)
		if err != nil {
			return Batch{}, false, approvals.ErrNotReady
		}
		if !slices.Equal(approvedHash, hash[:]) {
			return Batch{}, false, approvals.ErrNotReady
		}
		items, queryErr := store.ApprovalItems(ctx, request.ApprovalID)
		if queryErr != nil {
			return Batch{}, false, queryErr
		}
		var persisted []BatchItemRequest
		for _, item := range items {
			persisted = append(persisted, BatchItemRequest{NodeID: item.NodeID, Username: item.Username, Action: item.Action, ExpectedVersion: item.ExpectedVersion})
		}
		if len(persisted) != len(request.Items) || BatchRequestHash(persisted) != hash {
			return Batch{}, false, approvals.ErrNotReady
		}
		request.ID = approvedID
	}
	existing, existingHash, err := store.BatchByKey(ctx, request.WorkspaceID, request.IdempotencyKey)
	if err == nil {
		if (hasDisable(request.Items) && existing != request.ID) || !slices.Equal(existingHash, hash[:]) {
			return Batch{}, false, ErrIdempotencyConflict
		}
		if hasDisable(request.Items) {
			if err := approvals.ValidateConsumedBoundTx(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "user.batch.disable", "batch_operation", existing, hash[:]); err != nil {
				return Batch{}, false, err
			}
		}
		if err := commit(ctx, tx); err != nil {
			return Batch{}, false, err
		}
		batch, getErr := s.GetBatch(ctx, existing)
		return batch, true, getErr
	}
	if !errors.Is(err, database.ErrNotFound) {
		return Batch{}, false, err
	}
	id, now := request.ID, s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return Batch{}, false, err
	}
	if hasDisable(request.Items) {
		if err := approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "user.batch.disable", "batch_operation", id, hash[:]); err != nil {
			return Batch{}, false, err
		}
	}
	if err := store.InsertBatch(ctx, userstore.Batch{ID: id, WorkspaceID: request.WorkspaceID, ActorIdentityID: optionalUUID(request.ActorIdentityID), ActorSessionID: optionalUUID(request.ActorSessionID), ApprovalID: optionalUUID(request.ApprovalID), ActorID: request.ActorID, Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, IdempotencyKey: request.IdempotencyKey, Hash: hash[:], CreatedAt: at, UpdatedAt: at}); err != nil {
		return Batch{}, false, err
	}
	for index, item := range request.Items {
		state, errorType := "queued", ""
		if !item.Authorized {
			state, errorType = "forbidden", "forbidden"
		} else {
			exists, err := store.UserExists(ctx, item.NodeID, item.Username, request.WorkspaceID)
			if err != nil {
				return Batch{}, false, err
			}
			if !exists {
				state, errorType = "failed", "not_found"
			}
		}
		if err := store.InsertBatchItem(ctx, userstore.BatchItem{BatchID: id, Index: index, NodeID: item.NodeID, Username: item.Username, Action: item.Action, ExpectedVersion: item.ExpectedVersion, State: state, ErrorType: errorType, At: at}); err != nil {
			return Batch{}, false, err
		}
	}
	auditSummary, _ := json.Marshal(map[string]any{"request_hash": hex.EncodeToString(hash[:]), "items": request.Items})
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID, SessionID: optionalUUID(request.ActorSessionID), Action: "user.batch.create", ResourceType: "batch_operation", ResourceID: id, ApprovalID: optionalUUID(request.ApprovalID), RequestID: request.RequestID, TraceID: traceID(request.Traceparent), Reason: request.Reason, AfterSummary: auditSummary, At: now}); err != nil {
		return Batch{}, false, err
	}
	if err := commit(ctx, tx); err != nil {
		return Batch{}, false, err
	}
	batch, err := s.GetBatch(ctx, id)
	return batch, false, err
}

func (s *Service) GetBatch(ctx context.Context, id uuid.UUID) (Batch, error) {
	var batch Batch
	err := s.withStore(ctx, func(store userstore.Store) error {
		v, err := store.Batch(ctx, id)
		if err != nil {
			return err
		}
		batch = Batch{ID: id, WorkspaceID: v.WorkspaceID, ActorIdentityID: v.ActorIdentityID, State: v.State, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
		items, err := store.BatchItems(ctx, id)
		if err != nil {
			return err
		}
		for _, item := range items {
			batch.Items = append(batch.Items, BatchItem{Index: item.Index, NodeID: item.NodeID, Username: item.Username, Action: item.Action, ExpectedVersion: item.ExpectedVersion, State: item.State, ChildOperationID: item.ChildOperationID, ErrorType: item.ErrorType})
		}
		return nil
	})
	return batch, err
}

func (s *Service) Metrics(ctx context.Context, workspaceID uuid.UUID) (Metrics, error) {
	var result Metrics
	err := s.withStore(ctx, func(store userstore.Store) (err error) { result, err = store.Metrics(ctx, workspaceID); return })
	return result, err
}

// RunOnce obtains the database lease, compensates missed expiry/quota scans,
// submits at most the configured global command limit, and refreshes children.
func (s *Service) RunOnce(ctx context.Context) error {
	owner := s.newID()
	if fence := coordination.FenceFromContext(ctx); fence != nil {
		// Under fenced scheduling the leadership session replaces the
		// per-tick lease; every write below is fenced transactionally.
		if err := s.withStore(ctx, func(userstore.Store) error { return nil }); err != nil {
			return err
		}
	} else {
		acquired, err := s.acquireLease(ctx, owner, 25*time.Second)
		if err != nil || !acquired {
			return err
		}
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
	err := s.withStore(ctx, func(store userstore.Store) (err error) { count, err = store.ActiveOperations(ctx); return })
	return count, err
}

func (s *Service) resetMonthlyPolicies(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	now, err := value.FromTime(s.now())
	if err != nil {
		return 0, err
	}
	monthTime := monthStart(s.now())
	month, err := value.FromTime(monthTime)
	if err != nil {
		return 0, err
	}
	var candidates []userstore.Candidate
	err = s.withStore(ctx, func(store userstore.Store) (err error) {
		candidates, err = store.ResetCandidates(ctx, now, month, limit)
		return
	})
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, item := range candidates {
		key := stableKey("policy-reset", item.NodeID.String(), item.Username, fmt.Sprint(item.PolicyVersion), monthTime.Format(time.RFC3339))
		if err := s.withStore(ctx, func(store userstore.Store) error { return store.EnsureEnforcement(ctx, item, now) }); err != nil {
			return 0, err
		}
		if item.Enabled {
			operationID, found, findErr := s.findUserOperation(ctx, item.NodeID, item.Username, key, userstate.UserEnable)
			if findErr != nil {
				return 0, findErr
			}
			if !found {
				_ = s.withStore(ctx, func(store userstore.Store) error { return store.DeleteEnforcement(ctx, item, false) })
				continue
			}
			if err := s.withStore(ctx, func(store userstore.Store) error {
				return store.CompleteEnforcement(ctx, item, operationID, item.UserVersion)
			}); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.NodeID, Kind: userstate.UserEnable, Name: item.Username, ExpectedVersion: item.UserVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "monthly quota reset", RequestID: key, Traceparent: stableTraceparent(key)})
		if mutateErr != nil {
			if errors.Is(mutateErr, userstate.ErrBacklogExceeded) {
				return processed, nil
			}
			if errors.Is(mutateErr, userstate.ErrVersionConflict) || errors.Is(mutateErr, userstate.ErrRevisionPending) || errors.Is(mutateErr, userstate.ErrRevisionRecovery) {
				_ = s.withStore(ctx, func(store userstore.Store) error { return store.DeleteEnforcement(ctx, item, true) })
				continue
			}
			return 0, mutateErr
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return 0, parseErr
		}
		if err := s.withStore(ctx, func(store userstore.Store) error {
			return store.CompleteEnforcement(ctx, item, operationID, item.UserVersion+1)
		}); err != nil {
			return 0, err
		}
		processed++
	}
	return processed, nil
}

// A scheduler transaction commits only after checking its leadership term.
func (s *Service) withStore(ctx context.Context, change func(userstore.Store) error) error {
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := userstore.From(tx)
		if err != nil {
			return err
		}
		if err := change(store); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
}

func commit(ctx context.Context, tx database.Tx) error {
	if err := coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) acquireLease(ctx context.Context, owner uuid.UUID, duration time.Duration) (bool, error) {
	var acquired bool
	err := s.withStore(ctx, func(store userstore.Store) (err error) {
		acquired, err = store.AcquireLease(ctx, leaseName, owner, duration)
		return
	})
	return acquired, err
}

func (s *Service) enforcePolicies(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	now, err := value.FromTime(s.now())
	if err != nil {
		return 0, err
	}
	month, err := value.FromTime(monthStart(s.now()))
	if err != nil {
		return 0, err
	}
	var candidates []userstore.Candidate
	err = s.withStore(ctx, func(store userstore.Store) (err error) {
		candidates, err = store.EnforcementCandidates(ctx, now, month, limit)
		return
	})
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, item := range candidates {
		period, err := item.PeriodStart.Time()
		if err != nil {
			return 0, err
		}
		key := stableKey("policy", item.NodeID.String(), item.Username, fmt.Sprint(item.PolicyVersion), item.Cause, period.Format(time.RFC3339))
		trace := stableTraceparent(key)
		if err := s.withStore(ctx, func(store userstore.Store) error { return store.EnsureEnforcement(ctx, item, now) }); err != nil {
			return 0, err
		}
		if !item.Enabled {
			operationID, found, findErr := s.findUserOperation(ctx, item.NodeID, item.Username, key, userstate.UserDisable)
			if findErr != nil {
				return 0, findErr
			}
			if !found {
				_ = s.withStore(ctx, func(store userstore.Store) error { return store.DeleteEnforcement(ctx, item, false) })
				continue
			}
			if err := s.withStore(ctx, func(store userstore.Store) error {
				return store.CompleteEnforcement(ctx, item, operationID, item.UserVersion)
			}); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.NodeID, Kind: userstate.UserDisable, Name: item.Username, ExpectedVersion: item.UserVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: "scheduler", Reason: "quota or expiry policy enforcement", RequestID: key, Traceparent: trace})
		if mutateErr != nil {
			if errors.Is(mutateErr, userstate.ErrBacklogExceeded) {
				return processed, nil
			}
			if errors.Is(mutateErr, userstate.ErrVersionConflict) || errors.Is(mutateErr, userstate.ErrRevisionPending) || errors.Is(mutateErr, userstate.ErrRevisionRecovery) {
				_ = s.withStore(ctx, func(store userstore.Store) error { return store.DeleteEnforcement(ctx, item, true) })
				continue
			}
			return 0, mutateErr
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return 0, parseErr
		}
		if err := s.withStore(ctx, func(store userstore.Store) error {
			return store.CompleteEnforcement(ctx, item, operationID, item.UserVersion+1)
		}); err != nil {
			return 0, err
		}
		processed++
	}
	return processed, nil
}

func (s *Service) findUserOperation(ctx context.Context, nodeID uuid.UUID, username, key string, kind userstate.MutationKind) (uuid.UUID, bool, error) {
	var operationID uuid.UUID
	err := s.withStore(ctx, func(store userstore.Store) (err error) {
		operationID, err = store.FindUserOperation(ctx, nodeID, username, key, string(kind))
		return
	})
	if errors.Is(err, database.ErrNotFound) {
		return uuid.Nil, false, nil
	}
	return operationID, err == nil, err
}

// claimBatchItems claims queued batch items for submission. The claim is a
// write, so it commits only after the scheduler fencing assert succeeds.
func (s *Service) claimBatchItems(ctx context.Context, owner uuid.UUID, limit int) ([]userstore.BatchItem, error) {
	var items []userstore.BatchItem
	err := s.withStore(ctx, func(store userstore.Store) (err error) { items, err = store.ClaimBatchItems(ctx, owner, limit); return })
	return items, err
}

func (s *Service) submitBatchItems(ctx context.Context, owner uuid.UUID, limit int) error {
	items, err := s.claimBatchItems(ctx, owner, limit)
	if err != nil {
		return err
	}
	for _, item := range items {
		var batch userstore.Batch
		if err := s.withStore(ctx, func(store userstore.Store) (err error) { batch, err = store.Batch(ctx, item.BatchID); return }); err != nil {
			return err
		}
		kind := userstate.UserDisable
		if item.Action == "enable" {
			kind = userstate.UserEnable
		}
		key := stableKey("batch", item.BatchID.String(), fmt.Sprint(item.Index))
		op, _, mutateErr := s.users.Mutate(ctx, userstate.MutationRequest{NodeID: item.NodeID, Kind: kind, Name: item.Username, ExpectedVersion: item.ExpectedVersion, IdempotencyKey: key, TTL: 24 * time.Hour, ActorID: batch.ActorID, ActorIdentityID: derefUUID(batch.ActorIdentityID), ActorSessionID: derefUUID(batch.ActorSessionID), Reason: batch.Reason, RequestID: batch.RequestID + ":" + fmt.Sprint(item.Index), Traceparent: batch.Traceparent})
		if mutateErr != nil {
			if errors.Is(mutateErr, userstate.ErrBacklogExceeded) {
				err = s.withStore(ctx, func(store userstore.Store) error { return store.ReleaseBatchClaims(ctx, owner) })
				return err
			}
			err = s.withStore(ctx, func(store userstore.Store) error {
				return store.FinishBatchItem(ctx, item, owner, nil, userstateErrorType(mutateErr))
			})
			if err != nil {
				return err
			}
			continue
		}
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return parseErr
		}
		if err := s.withStore(ctx, func(store userstore.Store) error { return store.FinishBatchItem(ctx, item, owner, &operationID, "") }); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) refreshBatches(ctx context.Context) error {
	if err := s.withStore(ctx, func(store userstore.Store) error { return store.RefreshBatchItems(ctx, MaxBatchRefresh) }); err != nil {
		return err
	}
	return s.withStore(ctx, func(store userstore.Store) error { return store.RefreshBatches(ctx, MaxBatchRefresh) })
}

// RecordUsageTx converts monotonically increasing per-session counters into
// durable monthly and lifetime UTC usage without double-counting replays.
func RecordUsageTx(ctx context.Context, tx database.Tx, nodeID uuid.UUID, samples []UsageSample) error {
	err := userusage.RecordTransaction(ctx, tx, nodeID, samples)
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

func rollback(tx database.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
