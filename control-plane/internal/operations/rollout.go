package operations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/tracecontext"
	"github.com/google/uuid"
)

var (
	ErrRolloutInvalid     = errors.New("agent rollout request is invalid")
	ErrNoEligibleNodes    = errors.New("no eligible nodes for the agent rollout")
	ErrRolloutState       = errors.New("agent rollout state does not allow this transition")
	ErrRolloutUnavailable = errors.New("agent rollout orchestration is unavailable")
)

const (
	// The canary is always exactly one node; later batches are bounded so a
	// single advancement pass cannot flood the global command concurrency.
	rolloutMaxBatchSize = 20
	rolloutMaxNodes     = 500
	// Bounded work per advancement pass across the whole fleet.
	rolloutAdvanceLimit = 16
	// Dispatch claims expire so a crashed Controller cannot park a pending
	// node forever; the deterministic per-node idempotency key guarantees the
	// reclaim replays the exact same operation instead of duplicating it.
	rolloutDispatchLease = 30 * time.Second
	// Rollout-dispatched node upgrades reuse the single-node delivery bounds.
	rolloutUpgradeTTL = 300 * time.Second
)

// Rollout and node states mirror the agent_rollouts CHECK constraints. The
// failed/cancelled rollout states exist in the schema; the rollout coordinator
// only writes paused (awaiting an operator decision) or the two terminal states.
const (
	RolloutStateQueued    = "queued"
	RolloutStateRunning   = "running"
	RolloutStatePaused    = "paused"
	RolloutStateSucceeded = "succeeded"
	RolloutStateFailed    = "failed"

	RolloutNodePending    = "pending"
	RolloutNodeRunning    = "running"
	RolloutNodeSucceeded  = "succeeded"
	RolloutNodeFailed     = "failed"
	RolloutNodeRolledBack = "rolled_back"
	RolloutNodeUnknown    = "unknown"
	RolloutNodeSkipped    = "skipped"
)

// AgentRolloutNode is one selected node in stable sorted order. Batch 0 is
// the mandatory single-node canary.
type AgentRolloutNode struct {
	NodeID      string `json:"node_id"`
	Ordinal     int    `json:"ordinal"`
	Batch       int    `json:"batch"`
	State       string `json:"state"`
	OperationID string `json:"operation_id,omitempty"`
	FromVersion string `json:"from_version,omitempty"`
	FailureCode string `json:"failure_code,omitempty"`
}

// AgentRolloutExclusion explains why a requested node was not selected.
type AgentRolloutExclusion struct {
	NodeID string `json:"node_id"`
	Reason string `json:"reason"`
}

// AgentRollout is the durable fleet rollout read model. The Control Plane
// owns the canary and batch advancement; the browser never loops over
// per-node upgrade calls.
type AgentRollout struct {
	ID            string             `json:"id"`
	WorkspaceID   string             `json:"workspace_id"`
	TargetVersion string             `json:"target_version"`
	State         string             `json:"state"`
	BatchSize     int                `json:"batch_size"`
	StopOnFailure bool               `json:"stop_on_failure"`
	Reason        string             `json:"reason"`
	CreatedBy     string             `json:"created_by"`
	CurrentBatch  int                `json:"current_batch"`
	PauseCode     string             `json:"pause_code,omitempty"`
	CreatedAt     value.Timestamp    `json:"created_at"`
	UpdatedAt     value.Timestamp    `json:"updated_at"`
	Nodes         []AgentRolloutNode `json:"nodes,omitempty"`
	Excluded      []json.RawMessage  `json:"excluded,omitempty"`
}

type CreateAgentRolloutRequest struct {
	WorkspaceID     uuid.UUID
	TargetVersion   string
	NodeIDs         []uuid.UUID
	BatchSize       int
	StopOnFailure   bool
	Reason          string
	ActorID         string
	ActorIdentityID uuid.UUID
	ActorSessionID  uuid.UUID
	IdempotencyKey  string
	RequestID       string
	Traceparent     string
}

// rolloutRow is the durable rollout header read back inside the advancement
// and resume transactions.
type rolloutRow = operationstore.Rollout

type rolloutNodeRow = operationstore.RolloutNode

type claimedRolloutNode struct {
	rolloutNodeRow
	Observation rolloutNodeObservation
	Digest      [sha256.Size]byte
}

// rolloutNodeObservation is the durable evidence eligibility derives from.
type rolloutNodeObservation = operationstore.RolloutObservation

// rolloutNodeEligibility recomputes server-side eligibility from durable
// evidence only. The returned reason code is empty exactly when the node is
// eligible; the digest is the trusted release identity for the node's
// observed architecture. Browser-provided eligibility is never trusted.
func (s *Service) rolloutNodeEligibility(now time.Time, node rolloutNodeObservation, target string) (string, [sha256.Size]byte, bool) {
	switch {
	case node.Status != "active" && node.Status != "offline":
		return "not_trusted", [sha256.Size]byte{}, false
	case node.Status == "offline":
		return "offline", [sha256.Size]byte{}, false
	case !upgradeHeartbeatFresh(now, node.LastHeartbeatAt):
		return "stale", [sha256.Size]byte{}, false
	case node.Architecture == "":
		return "missing_release_metadata", [sha256.Size]byte{}, false
	}
	digest, trusted := s.releaseCatalog.Lookup(target, node.Architecture)
	if !trusted {
		return "missing_release_metadata", [sha256.Size]byte{}, false
	}
	if !node.CapabilityOK {
		return "missing_capability", [sha256.Size]byte{}, false
	}
	if node.UpgradeActive {
		return "upgrade_in_progress", [sha256.Size]byte{}, false
	}
	return "", digest, true
}

// rolloutBatchForOrdinal assigns the mandatory canary (batch 0, ordinal 0)
// and bounds every later batch to at most batch_size ordinals.
func rolloutBatchForOrdinal(ordinal, batchSize int) int {
	if ordinal == 0 {
		return 0
	}
	return 1 + (ordinal-1)/batchSize
}

func sortAndDedupeNodeIDs(nodeIDs []uuid.UUID) ([]uuid.UUID, error) {
	seen := make(map[uuid.UUID]struct{}, len(nodeIDs))
	sorted := make([]uuid.UUID, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == uuid.Nil || id.Version() != 7 {
			return nil, ErrRolloutInvalid
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		sorted = append(sorted, id)
	}
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return sorted, nil
}

func nodeIDStrings(ids []uuid.UUID) []string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, id.String())
	}
	return values
}

func agentRolloutRequestHash(target string, sorted []uuid.UUID, batchSize int, stopOnFailure bool) []byte {
	summary, _ := json.Marshal(struct {
		TargetVersion string      `json:"target_version"`
		NodeIDs       []uuid.UUID `json:"node_ids"`
		BatchSize     int         `json:"batch_size"`
		StopOnFailure bool        `json:"stop_on_failure"`
	}{target, sorted, batchSize, stopOnFailure})
	digest := sha256.Sum256(append([]byte("ocservia/agent-rollout/v1\x00"), summary...))
	return digest[:]
}

func rolloutFromRow(rollout rolloutRow, nodes []AgentRolloutNode) AgentRollout {
	return AgentRollout{
		ID: rollout.ID.String(), WorkspaceID: rollout.WorkspaceID.String(), TargetVersion: rollout.TargetVersion,
		State: rollout.State, BatchSize: rollout.BatchSize, StopOnFailure: rollout.StopOnFailure,
		Reason: rollout.Reason, CreatedBy: rollout.CreatedBy.String(),
		CurrentBatch: rollout.CurrentBatch, PauseCode: rollout.PauseCode,
		CreatedAt: rollout.CreatedAt, UpdatedAt: rollout.UpdatedAt, Nodes: nodes, Excluded: rollout.Excluded,
	}
}

// rolloutTraceparent derives a stable, valid traceparent from the rollout
// and node identities so every dispatch attempt of the same node shares one
// trace.
func rolloutTraceparent(rolloutID, nodeID uuid.UUID) string {
	digest := sha256.Sum256(append(rolloutID[:], nodeID[:]...))
	return fmt.Sprintf("00-%032x-%016x-01", digest[:16], digest[16:24])
}

func loadRolloutNodeObservation(ctx context.Context, store operationstore.Store, nodeID, workspaceID uuid.UUID) (rolloutNodeObservation, error) {
	node, err := store.RolloutObservation(ctx, nodeID, workspaceID)
	if errors.Is(err, database.ErrNotFound) {
		return node, ErrRolloutInvalid
	}
	return node, err
}

// GetAgentRollout reads the rollout header and its nodes in stable ordinal order.
func (s *Service) GetAgentRollout(ctx context.Context, rolloutID uuid.UUID) (AgentRollout, error) {
	var result AgentRollout
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		rollout, err := store.Rollout(ctx, rolloutID, false)
		if err != nil {
			return err
		}
		nodes, err := store.RolloutNodes(ctx, rolloutID)
		if err != nil {
			return err
		}
		result = rolloutFromRow(rollout, rolloutNodeModels(nodes))
		return nil
	})
	return result, err
}

// ListAgentRollouts returns the workspace's most recent rollouts with their
// node projections so the fleet view can show bounded progress summaries.
func (s *Service) ListAgentRollouts(ctx context.Context, workspaceID uuid.UUID, limit int) ([]AgentRollout, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rollouts := []AgentRollout{}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		rows, err := store.Rollouts(ctx, workspaceID, limit)
		if err != nil {
			return err
		}
		for _, rollout := range rows {
			nodes, err := store.RolloutNodes(ctx, rollout.ID)
			if err != nil {
				return err
			}
			rollouts = append(rollouts, rolloutFromRow(rollout, rolloutNodeModels(nodes)))
		}
		return nil
	})
	return rollouts, err
}

// RolloutWorkspace resolves the owning workspace of a rollout for
// workspace-scoped request authorization.
func (s *Service) RolloutWorkspace(ctx context.Context, rolloutID uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		workspaceID, err = store.RolloutWorkspace(ctx, rolloutID)
		return err
	})
	return workspaceID, err
}

func rolloutNodeModels(rows []operationstore.RolloutNode) []AgentRolloutNode {
	nodes := []AgentRolloutNode{}
	for _, row := range rows {
		node := AgentRolloutNode{NodeID: row.NodeID.String(), Ordinal: row.Ordinal, Batch: row.Batch, State: row.State, FromVersion: row.FromVersion, FailureCode: row.FailureCode}
		if row.OperationID != uuid.Nil {
			node.OperationID = row.OperationID.String()
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func (s *Service) loadRollout(ctx context.Context, id uuid.UUID) (rolloutRow, error) {
	var row rolloutRow
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		row, err = store.Rollout(ctx, id, false)
		return err
	})
	return row, err
}

// CreateAgentRollout durably records a canary and rolling agent upgrade
// rollout after verifying session and workspace administrator authorization.
func (s *Service) CreateAgentRollout(ctx context.Context, request CreateAgentRolloutRequest) (AgentRollout, bool, error) {
	if s.releaseCatalog == nil {
		return AgentRollout{}, false, ErrRolloutUnavailable
	}
	target := strings.TrimSpace(request.TargetVersion)
	reason := strings.TrimSpace(request.Reason)
	if !semanticpayload.ValidAgentUpgradeTargetVersion(target) || reason == "" || len(reason) > 512 ||
		request.BatchSize < 1 || request.BatchSize > rolloutMaxBatchSize || !request.StopOnFailure ||
		request.WorkspaceID == uuid.Nil ||
		request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return AgentRollout{}, false, ErrRolloutInvalid
	}
	sorted, err := sortAndDedupeNodeIDs(request.NodeIDs)
	if err != nil || len(sorted) == 0 || len(sorted) > rolloutMaxNodes {
		return AgentRollout{}, false, ErrRolloutInvalid
	}
	requestHash := agentRolloutRequestHash(target, sorted, request.BatchSize, request.StopOnFailure)
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return AgentRollout{}, false, err
	}
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return AgentRollout{}, false, fmt.Errorf("begin agent rollout creation: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	store, err := operationstore.FromTransaction(tx)
	if err != nil {
		return AgentRollout{}, false, err
	}
	if err := auth.AuthorizeMutationTx(ctx, tx, request.ActorIdentityID, request.ActorSessionID, request.WorkspaceID); err != nil {
		return AgentRollout{}, false, err
	}
	existingID, existingHash, err := store.FindRollout(ctx, request.WorkspaceID, request.IdempotencyKey)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return AgentRollout{}, false, err
	} else if err == nil {
		if string(existingHash) != string(requestHash) {
			return AgentRollout{}, false, ErrIdempotencyConflict
		}
		rollout, loadErr := store.Rollout(ctx, existingID, false)
		if loadErr != nil {
			return AgentRollout{}, false, loadErr
		}
		nodes, loadErr := store.RolloutNodes(ctx, existingID)
		if loadErr != nil {
			return AgentRollout{}, false, loadErr
		}
		return rolloutFromRow(rollout, rolloutNodeModels(nodes)), true, nil
	}
	rolloutID := uuid.Must(uuid.NewV7())
	eligible, exclusions, err := s.selectRolloutNodes(ctx, store, request.WorkspaceID, sorted, target, now)
	if err != nil {
		return AgentRollout{}, false, err
	}
	exclusionsJSON, err := json.Marshal(exclusions)
	if err != nil {
		return AgentRollout{}, false, err
	}
	var storedExclusions []json.RawMessage
	if err := json.Unmarshal(exclusionsJSON, &storedExclusions); err != nil {
		return AgentRollout{}, false, err
	}
	auditID, err := uuid.NewV7()
	if err != nil {
		return AgentRollout{}, false, err
	}
	if err := store.InsertRollout(ctx, operationstore.PendingRollout{Rollout: operationstore.Rollout{ID: rolloutID, WorkspaceID: request.WorkspaceID, TargetVersion: target, BatchSize: request.BatchSize, Reason: reason, RequestHash: requestHash, CreatedBy: request.ActorIdentityID, ActorSession: request.ActorSessionID, Excluded: storedExclusions, CreatedAt: at}, IdempotencyKey: request.IdempotencyKey}); err != nil {
		return AgentRollout{}, false, fmt.Errorf("insert agent rollout: %w", err)
	}
	for ordinal, node := range eligible {
		if err := store.InsertRolloutNode(ctx, rolloutID, operationstore.RolloutNode{NodeID: node.nodeID, Ordinal: ordinal, Batch: rolloutBatchForOrdinal(ordinal, request.BatchSize)}, at); err != nil {
			return AgentRollout{}, false, fmt.Errorf("insert agent rollout node: %w", err)
		}
	}
	summary, _ := json.Marshal(map[string]any{
		"target_version": target, "batch_size": request.BatchSize, "stop_on_failure": true,
		"node_count": len(eligible), "node_ids": nodeIDStrings(sorted), "excluded": exclusions,
	})
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{
		EventID: auditID, WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID,
		SessionID: &request.ActorSessionID, Action: "agent.rollout", ResourceType: "agent_rollout",
		ResourceID: rolloutID, RequestID: request.RequestID,
		TraceID: tracecontext.TraceID(request.Traceparent), Reason: reason, Result: "intent", AfterSummary: summary, At: now,
	}); err != nil {
		return AgentRollout{}, false, fmt.Errorf("append agent rollout audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return AgentRollout{}, false, fmt.Errorf("commit agent rollout creation: %w", err)
	}
	rollout, err := s.GetAgentRollout(ctx, rolloutID)
	if err != nil {
		return AgentRollout{}, false, err
	}
	return rollout, false, nil
}

// ResumeAgentRollout records an explicit operator decision to continue a
// paused rollout. Succeeded nodes are never redispatched; failed, unknown,
// and rolled-back nodes of the current batch are requeued for a fresh
// eligibility check and upgrade attempt. A skipped canary is also requeued:
// no operator decision may replace the mandatory successful canary.
func (s *Service) ResumeAgentRollout(ctx context.Context, rolloutID uuid.UUID, actorID string, actorIdentityID, actorSessionID uuid.UUID, requestID, traceparent string) (AgentRollout, error) {
	if actorIdentityID == uuid.Nil || actorSessionID == uuid.Nil {
		return AgentRollout{}, ErrRolloutInvalid
	}
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return AgentRollout{}, err
	}
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return AgentRollout{}, fmt.Errorf("begin agent rollout resume: %w", err)
	}
	defer rollbackRollout(tx)
	rollout, err := q.Rollout(ctx, rolloutID, true)
	if err != nil {
		return AgentRollout{}, err
	}
	if rollout.State != RolloutStatePaused {
		return AgentRollout{}, ErrRolloutState
	}
	requeued, err := q.ResumeRollout(ctx, rolloutID, rollout.CurrentBatch, at)
	if err != nil {
		return AgentRollout{}, fmt.Errorf("requeue failed rollout nodes: %w", err)
	}
	summary, _ := json.Marshal(map[string]any{"event": "rollout_resumed", "current_batch": rollout.CurrentBatch, "requeued_nodes": requeued})
	auditID, err := uuid.NewV7()
	if err != nil {
		return AgentRollout{}, err
	}
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{
		EventID: auditID, WorkspaceID: rollout.WorkspaceID, ActorType: "user", ActorID: actorID,
		SessionID: &actorSessionID, Action: "agent.rollout", ResourceType: "agent_rollout",
		ResourceID: rollout.ID, RequestID: requestID, TraceID: tracecontext.TraceID(traceparent),
		AfterSummary: summary, At: now,
	}); err != nil {
		return AgentRollout{}, fmt.Errorf("append agent rollout resume audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentRollout{}, fmt.Errorf("commit agent rollout resume: %w", err)
	}
	return s.GetAgentRollout(ctx, rolloutID)
}

type eligibleRolloutNode struct {
	nodeID uuid.UUID
	digest [sha256.Size]byte
}

func (s *Service) selectRolloutNodes(ctx context.Context, store operationstore.Store, workspaceID uuid.UUID, sorted []uuid.UUID, target string, now time.Time) ([]eligibleRolloutNode, []AgentRolloutExclusion, error) {
	exclusions := make([]AgentRolloutExclusion, 0, len(sorted))
	eligible := make([]eligibleRolloutNode, 0, len(sorted))
	for _, nodeID := range sorted {
		node, loadErr := store.RolloutObservation(ctx, nodeID, workspaceID)
		if errors.Is(loadErr, database.ErrNotFound) {
			return nil, nil, ErrRolloutInvalid
		}
		if loadErr != nil {
			return nil, nil, loadErr
		}
		exclusionReason, digest, ok := s.rolloutNodeEligibility(now, node, target)
		if !ok {
			exclusions = append(exclusions, AgentRolloutExclusion{NodeID: nodeID.String(), Reason: exclusionReason})
			continue
		}
		eligible = append(eligible, eligibleRolloutNode{nodeID: nodeID, digest: digest})
	}
	if len(eligible) == 0 {
		return nil, nil, ErrNoEligibleNodes
	}
	return eligible, exclusions, nil
}
