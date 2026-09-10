package operations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

// pendingUpgrade is one scheduled single-node agent upgrade together with the
// durable Controller-side evidence needed to reconcile it.
type pendingUpgrade = operationstore.ScheduledUpgrade

// agentUpgradeDecision is the terminal or progress conclusion for one
// scheduled upgrade derived only from durable Controller-side evidence.
type agentUpgradeDecision struct {
	State    string
	Terminal bool
	Reason   string
}

// ReconcileAgentUpgrades advances the family-specific lifecycle of scheduled
// single-node agent upgrades. A disconnect during the expected restart window
// is normal progress, never immediate failure, and a reconnect alone never
// proves success: terminal success additionally needs the durable local
// outcome and a fresh observation of the target version.
func (s *Service) ReconcileAgentUpgrades(ctx context.Context) error {
	var pending []pendingUpgrade
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		pending, err = store.PendingUpgrades(ctx, reconciliationBatchLimit)
		return err
	})
	if err != nil {
		return fmt.Errorf("scan pending agent upgrades: %w", err)
	}
	for _, upgrade := range pending {
		decision, changed := s.decideAgentUpgrade(s.now(), upgrade)
		if !changed {
			continue
		}
		if decision.Terminal {
			if err := s.applyAgentUpgradeTerminal(ctx, upgrade, decision); err != nil {
				return err
			}
			continue
		}
		if err := s.applyAgentUpgradeProgress(ctx, upgrade, decision); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) decideAgentUpgrade(now time.Time, upgrade pendingUpgrade) (agentUpgradeDecision, bool) {
	// The generic engine already closed the operation (expiry or supersede);
	// only the family projection lags behind. Its state set has no
	// expired/superseded members: an expired scheduling attempt leaves the
	// outcome unknown, a superseded one definitively did not run.
	if upgrade.OperationState == "expired" {
		return agentUpgradeDecision{State: "unknown", Terminal: true, Reason: "operation_expired"}, true
	}
	if upgrade.OperationState == "superseded" {
		return agentUpgradeDecision{State: "failed", Terminal: true, Reason: "operation_superseded"}, true
	}
	if upgrade.State == "queued" {
		// The scheduling acknowledgement has not arrived; the outbox
		// machinery and command expiry own this phase.
		return agentUpgradeDecision{}, false
	}
	online := upgrade.NodeStatus == "active" && upgradeHeartbeatFresh(now, upgrade.LastHeartbeatAt)
	deadline, err := value.FromTime(now.Add(-s.agentUpgradeReconcileTime))
	if err != nil {
		return agentUpgradeDecision{}, false
	}
	if now.Nanosecond()%1000 != 0 {
		deadline.Micros++
	}
	switch {
	case upgrade.DurableState == "failed":
		return agentUpgradeDecision{State: "failed", Terminal: true, Reason: "durable_local_failure"}, true
	case upgrade.DurableState == "rolled_back":
		return agentUpgradeDecision{State: "rolled_back", Terminal: true, Reason: "durable_local_rollback"}, true
	case upgrade.DurableState == "succeeded" && online && upgrade.ObservedVersion == upgrade.TargetVersion:
		return agentUpgradeDecision{State: "succeeded", Terminal: true, Reason: "durable_success_and_target_version_observed"}, true
	case upgrade.ScheduledAt.Valid && upgrade.ScheduledAt.Micros < deadline.Micros:
		return agentUpgradeDecision{State: "unknown", Terminal: true, Reason: "reconciliation_deadline_exceeded"}, true
	case upgrade.State == "accepted" && online && upgrade.ObservedAt.Valid && upgrade.ScheduledAt.Valid && upgrade.ObservedAt.Micros > upgrade.ScheduledAt.Micros:
		return agentUpgradeDecision{State: "running", Terminal: false, Reason: "node_reconnected_verifying_target_version"}, true
	}
	return agentUpgradeDecision{}, false
}

func upgradeHeartbeatFresh(now time.Time, heartbeat value.Timestamp) bool {
	cutoff, err := value.FromTime(now.Add(-telemetry.OfflineAfter))
	if err != nil || !heartbeat.Valid {
		return false
	}
	if now.Nanosecond()%1000 != 0 {
		cutoff.Micros++
	}
	return heartbeat.Micros >= cutoff.Micros
}

func (s *Service) applyAgentUpgradeTerminal(ctx context.Context, upgrade pendingUpgrade, decision agentUpgradeDecision) error {
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return err
	}
	commandState := decision.State
	if commandState == "unknown" {
		// No terminal command result will ever arrive; expire the delivery
		// lifecycle so the generic recovery loops stop re-dispatching.
		commandState = "expired"
	}
	// A generic-engine terminal (expired or superseded) already closed the
	// operation, command, and outbox; only the projection lags behind.
	genericTerminal := upgrade.OperationState == "expired" || upgrade.OperationState == "superseded"
	unchanged := false
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.UpgradeTerminal(ctx, operationstore.UpgradeTransition{OperationID: upgrade.OperationID, CommandID: upgrade.CommandID, State: decision.State, OperationState: upgrade.OperationState, CommandState: commandState, GenericTerminal: genericTerminal, At: at}); err != nil {
			unchanged = errors.Is(err, database.ErrNotFound)
			return err
		}
		auditID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		auditResult := "failed"
		if decision.State == "succeeded" {
			auditResult = "succeeded"
		}
		summary, _ := json.Marshal(map[string]any{
			"terminal_outcome": decision.State,
			"from_version":     upgrade.FromVersion,
			"target_version":   upgrade.TargetVersion,
			"observed_version": upgrade.ObservedVersion,
			"durable_state":    upgrade.DurableState,
			"reason":           decision.Reason,
		})
		if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			EventID: auditID, WorkspaceID: upgrade.WorkspaceID, ActorType: "controller", ActorID: "agent-upgrade-reconciler",
			Action: "agent.upgrade", ResourceType: "operation", ResourceID: upgrade.OperationID,
			NodeID: &upgrade.NodeID, CommandID: &upgrade.CommandID, Result: auditResult,
			AfterSummary: summary, At: now,
		}); err != nil {
			return fmt.Errorf("append agent upgrade outcome audit: %w", err)
		}
		return nil
	})
	if unchanged {
		return nil
	}
	return err
}

func (s *Service) applyAgentUpgradeProgress(ctx context.Context, upgrade pendingUpgrade, decision agentUpgradeDecision) error {
	at, err := value.FromTime(s.now())
	if err != nil {
		return err
	}
	unchanged := false
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		err = store.UpgradeProgress(ctx, operationstore.UpgradeTransition{OperationID: upgrade.OperationID, State: decision.State, At: at})
		unchanged = errors.Is(err, database.ErrNotFound)
		return err
	})
	if unchanged {
		return nil
	}
	return err
}

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
// failed/cancelled rollout states exist in the schema; P0 only ever writes
// paused (awaiting an explicit operator decision) or the two terminal states.
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
	ApprovalID    string             `json:"approval_id"`
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
	ApprovalID      uuid.UUID
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
	var reason string
	switch telemetry.ClassifyAgentVersion(node.AgentVersion, target) {
	case telemetry.AgentVersionStateUnknown:
		reason = "unknown_version"
	case telemetry.AgentVersionStateCurrent:
		reason = "already_current"
	case telemetry.AgentVersionStateAhead:
		reason = "ahead"
	case telemetry.AgentVersionStateUpgradeAvailable:
		reason = ""
	}
	if reason != "" {
		return reason, [sha256.Size]byte{}, false
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

func rolloutFromRow(rollout rolloutRow, nodes []AgentRolloutNode) AgentRollout {
	return AgentRollout{
		ID: rollout.ID.String(), WorkspaceID: rollout.WorkspaceID.String(), TargetVersion: rollout.TargetVersion,
		State: rollout.State, BatchSize: rollout.BatchSize, StopOnFailure: rollout.StopOnFailure,
		Reason: rollout.Reason, ApprovalID: rollout.ApprovalID.String(), CreatedBy: rollout.CreatedBy.String(),
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
// rollout. Server-side eligibility recompute excludes ineligible nodes with
// a reason, the immutable rollout request is bound to the consumed approval,
// and the rollout id comes from the approval itself so the binding cannot be
// replayed for a different rollout.
func (s *Service) CreateAgentRollout(ctx context.Context, request CreateAgentRolloutRequest) (AgentRollout, bool, error) {
	if s.releaseCatalog == nil {
		return AgentRollout{}, false, ErrRolloutUnavailable
	}
	target := strings.TrimSpace(request.TargetVersion)
	reason := strings.TrimSpace(request.Reason)
	if !semanticpayload.ValidAgentUpgradeTargetVersion(target) || reason == "" || len(reason) > 512 ||
		request.BatchSize < 1 || request.BatchSize > rolloutMaxBatchSize || !request.StopOnFailure ||
		request.WorkspaceID == uuid.Nil || request.ApprovalID.Version() != 7 ||
		request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil ||
		request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return AgentRollout{}, false, ErrRolloutInvalid
	}
	sorted, err := sortAndDedupeNodeIDs(request.NodeIDs)
	if err != nil || len(sorted) == 0 || len(sorted) > rolloutMaxNodes {
		return AgentRollout{}, false, ErrRolloutInvalid
	}
	requestHash, _ := approvals.AgentRolloutBinding(target, sorted, request.BatchSize, request.StopOnFailure)
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
	// The approval row carries the rollout identity: the requester pinned the
	// exact rollout request at approval time and consumption binds this
	// rollout to it. A reused, mismatched, or unapproved approval fails
	// closed.
	approvedResource, err := store.RolloutApproval(ctx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, requestHash, at)
	if errors.Is(err, database.ErrNotFound) {
		return AgentRollout{}, false, approvals.ErrNotReady
	}
	if err != nil {
		return AgentRollout{}, false, fmt.Errorf("lock rollout approval: %w", err)
	}
	if approvedResource.Version() != 7 {
		return AgentRollout{}, false, approvals.ErrNotReady
	}
	rolloutID := approvedResource
	exclusions := make([]AgentRolloutExclusion, 0, len(sorted))
	type eligibleNode struct {
		nodeID uuid.UUID
		digest [sha256.Size]byte
	}
	eligible := make([]eligibleNode, 0, len(sorted))
	for _, nodeID := range sorted {
		node, loadErr := store.RolloutObservation(ctx, nodeID, request.WorkspaceID)
		if errors.Is(loadErr, database.ErrNotFound) {
			return AgentRollout{}, false, ErrRolloutInvalid
		}
		if loadErr != nil {
			return AgentRollout{}, false, loadErr
		}
		exclusionReason, digest, ok := s.rolloutNodeEligibility(now, node, target)
		if !ok {
			exclusions = append(exclusions, AgentRolloutExclusion{NodeID: nodeID.String(), Reason: exclusionReason})
			continue
		}
		eligible = append(eligible, eligibleNode{nodeID: nodeID, digest: digest})
	}
	if len(eligible) == 0 {
		return AgentRollout{}, false, ErrNoEligibleNodes
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
	if err := store.InsertRollout(ctx, operationstore.PendingRollout{Rollout: operationstore.Rollout{ID: rolloutID, WorkspaceID: request.WorkspaceID, TargetVersion: target, BatchSize: request.BatchSize, Reason: reason, ApprovalID: request.ApprovalID, RequestHash: requestHash, CreatedBy: request.ActorIdentityID, ActorSession: request.ActorSessionID, Excluded: storedExclusions, CreatedAt: at}, IdempotencyKey: request.IdempotencyKey}); err != nil {
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
		ResourceID: rolloutID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID,
		TraceID: traceID(request.Traceparent), Reason: reason, Result: "intent", AfterSummary: summary, At: now,
	}); err != nil {
		return AgentRollout{}, false, fmt.Errorf("append agent rollout audit: %w", err)
	}
	if err := approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "agent.rollout", "batch_operation", rolloutID, requestHash[:]); err != nil {
		return AgentRollout{}, false, err
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
		ResourceID: rollout.ID, RequestID: requestID, TraceID: traceID(traceparent),
		AfterSummary: summary, At: now,
	}); err != nil {
		return AgentRollout{}, fmt.Errorf("append agent rollout resume audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentRollout{}, fmt.Errorf("commit agent rollout resume: %w", err)
	}
	return s.GetAgentRollout(ctx, rolloutID)
}

// AdvanceAgentRollouts advances queued and running rollouts by at most one
// durable step per rollout: roll up terminal node operations, pause on the
// first failure or unknown outcome, advance the batch pointer, and dispatch
// the pending nodes of the current batch through the reconciled single-node
// upgrade operation. It is safe to run concurrently: every rollout is
// advanced under its own row lock.
func (s *Service) AdvanceAgentRollouts(ctx context.Context) error {
	if s.releaseCatalog == nil {
		return nil
	}
	rolloutIDs := []uuid.UUID{}
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := operationstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		rolloutIDs, err = store.ActiveRollouts(ctx, rolloutAdvanceLimit)
		return err
	})
	if err != nil {
		return err
	}
	var advanceErrs []error
	for _, rolloutID := range rolloutIDs {
		if err := s.advanceAgentRollout(ctx, rolloutID); err != nil {
			advanceErrs = append(advanceErrs, fmt.Errorf("advance rollout %s: %w", rolloutID, err))
		}
	}
	return errors.Join(advanceErrs...)
}

func (s *Service) advanceAgentRollout(ctx context.Context, rolloutID uuid.UUID) error {
	// Bounded cascade: a completed batch hands control to the next batch
	// within the same pass so batch pacing never waits for the next tick.
	for pass := 0; pass < 3; pass++ {
		claimed, progressed, err := s.prepareRolloutAdvance(ctx, rolloutID)
		if err != nil {
			return err
		}
		if !progressed {
			return nil
		}
		for _, node := range claimed {
			stop, err := s.dispatchRolloutNode(ctx, rolloutID, node)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
	}
	return nil
}

// prepareRolloutAdvance runs the locked evaluation phase: roll up terminal
// node operations, enforce stop-on-first-failure/unknown, advance the batch
// pointer, claim dispatchable pending nodes, and record skipped nodes. The
// returned claims are dispatched outside the lock because node upgrade
// creation manages its own transaction; progressed reports whether the
// rollout moved forward and another pass may make further progress.
func (s *Service) prepareRolloutAdvance(ctx context.Context, rolloutID uuid.UUID) ([]claimedRolloutNode, bool, error) {
	now := s.now()
	at, err := value.FromTime(now)
	if err != nil {
		return nil, false, err
	}
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin rollout advance: %w", err)
	}
	defer rollbackRollout(tx)
	rollout, err := q.Rollout(ctx, rolloutID, true)
	if err != nil {
		return nil, false, err
	}
	if rollout.State != RolloutStateQueued && rollout.State != RolloutStateRunning {
		return nil, false, nil
	}
	progressed := false
	events := []audit.ChainRecord{}
	appendEvent := func(summary map[string]any) error {
		eventID, eventErr := uuid.NewV7()
		if eventErr != nil {
			return eventErr
		}
		encoded, _ := json.Marshal(summary)
		events = append(events, audit.ChainRecord{
			EventID: eventID, WorkspaceID: rollout.WorkspaceID, ActorType: "controller", ActorID: "agent-rollout-orchestrator",
			Action: "agent.rollout", ResourceType: "agent_rollout", ResourceID: rollout.ID, AfterSummary: encoded, At: now,
		})
		return nil
	}
	if rollout.State == RolloutStateQueued {
		if err := q.SetRolloutState(ctx, rolloutID, "running", rollout.PauseCode, at); err != nil {
			return nil, false, fmt.Errorf("start rollout: %w", err)
		}
		rollout.State = RolloutStateRunning
		progressed = true
		if err := appendEvent(map[string]any{"event": "batch_started", "batch": 0, "canary": true}); err != nil {
			return nil, false, err
		}
	}
	if err := s.rollUpTerminalRolloutNodes(ctx, q, &rollout, now, appendEvent); err != nil {
		return nil, false, err
	}
	claimed, advanced, err := s.evaluateRolloutBatch(ctx, q, &rollout, now, appendEvent)
	if err != nil {
		return nil, false, err
	}
	if advanced || len(claimed) > 0 {
		progressed = true
	}
	for _, event := range events {
		if err := audit.AppendChainTx(ctx, tx, event); err != nil {
			return nil, false, fmt.Errorf("append rollout audit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit rollout advance: %w", err)
	}
	return claimed, progressed, nil
}

// rollUpTerminalRolloutNodes copies terminal single-node upgrade outcomes
// into the rollout projection. The rollout never derives its own success
// rule: a node succeeds only when its reconciled single-node operation
// succeeded (durable outcome, online, fresh telemetry, target version
// observed).
func (s *Service) rollUpTerminalRolloutNodes(ctx context.Context, q operationstore.Store, rollout *rolloutRow, now time.Time, appendEvent func(map[string]any) error) error {
	at, err := value.FromTime(now)
	if err != nil {
		return err
	}
	terminal, err := q.TerminalRolloutNodes(ctx, rollout.ID)
	if err != nil {
		return fmt.Errorf("read terminal rollout nodes: %w", err)
	}
	for _, node := range terminal {
		if !slices.Contains([]string{"succeeded", "failed", "rolled_back", "unknown"}, node.Outcome) {
			// The operation lifecycle closed without a reconciled upgrade
			// outcome (expired command); the upgrade outcome is unknown.
			node.Outcome = RolloutNodeUnknown
		}
		failureCode := ""
		switch node.Outcome {
		case RolloutNodeFailed:
			failureCode = "upgrade_failed"
		case RolloutNodeRolledBack:
			failureCode = "upgrade_rolled_back"
		case RolloutNodeUnknown:
			failureCode = "outcome_unknown"
		}
		changed, err := q.SetRolloutNodeOutcome(ctx, rollout.ID, node.NodeID, node.Outcome, failureCode, at)
		if err != nil {
			return fmt.Errorf("roll up rollout node outcome: %w", err)
		}
		if !changed {
			continue
		}
		if node.Outcome != RolloutNodeSucceeded {
			if err := appendEvent(map[string]any{
				"event": "node_" + node.Outcome, "batch": node.Batch, "node_id": node.NodeID.String(),
				"operation_id": node.OperationID.String(), "failure_code": failureCode,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluateRolloutBatch enforces stop-on-first-failure/unknown, advances the
// batch pointer, and claims the dispatchable pending nodes of the current
// batch. A skipped node can only be observed here after an operator resumes
// the rollout; it never counts as an upgrade.
// The second return reports whether the batch pointer advanced.
func (s *Service) evaluateRolloutBatch(ctx context.Context, q operationstore.Store, rollout *rolloutRow, now time.Time, appendEvent func(map[string]any) error) ([]claimedRolloutNode, bool, error) {
	at, err := value.FromTime(now)
	if err != nil {
		return nil, false, err
	}
	nodes, err := q.RolloutNodes(ctx, rollout.ID)
	if err != nil {
		return nil, false, err
	}
	var blocked *rolloutNodeRow
	for index := range nodes {
		if nodes[index].State == RolloutNodeFailed || nodes[index].State == RolloutNodeUnknown || nodes[index].State == RolloutNodeRolledBack {
			blocked = &nodes[index]
			break
		}
	}
	if blocked != nil && rollout.StopOnFailure {
		// P0 fixes the stop-on-failure policy to true; the stored policy is
		// part of the approved rollout request hash.
		pauseCode := "node_" + blocked.State
		if err := q.SetRolloutState(ctx, rollout.ID, "paused", pauseCode, at); err != nil {
			return nil, false, fmt.Errorf("pause rollout: %w", err)
		}
		rollout.State = RolloutStatePaused
		if err := appendEvent(map[string]any{"event": "rollout_paused", "pause_code": pauseCode, "batch": blocked.Batch, "node_id": blocked.NodeID.String(), "node_state": blocked.State}); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	var current []*rolloutNodeRow
	pendingElsewhere := false
	for index := range nodes {
		switch {
		case nodes[index].Batch == rollout.CurrentBatch:
			current = append(current, &nodes[index])
		case nodes[index].State == RolloutNodePending || nodes[index].State == RolloutNodeRunning:
			pendingElsewhere = true
		}
	}
	batchComplete := len(current) > 0
	for _, node := range current {
		if node.State == RolloutNodePending || node.State == RolloutNodeRunning {
			batchComplete = false
			break
		}
	}
	if !batchComplete {
		claimed, err := s.claimRolloutPendingNodes(ctx, q, rollout, current, now, appendEvent)
		return claimed, len(claimed) > 0, err
	}
	if pendingElsewhere {
		nextBatch := rollout.CurrentBatch + 1
		if err := q.SetRolloutBatch(ctx, rollout.ID, nextBatch, at); err != nil {
			return nil, false, fmt.Errorf("advance rollout batch: %w", err)
		}
		if err := appendEvent(map[string]any{"event": "batch_completed", "batch": rollout.CurrentBatch}); err != nil {
			return nil, false, err
		}
		rollout.CurrentBatch = nextBatch
		if err := appendEvent(map[string]any{"event": "batch_started", "batch": nextBatch, "canary": false}); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	// Every node reached a terminal state: succeeded nodes count as upgrades,
	// skipped nodes do not. A rollout that upgraded nothing failed.
	succeeded, skipped := 0, 0
	for _, node := range nodes {
		switch node.State {
		case RolloutNodeSucceeded:
			succeeded++
		case RolloutNodeSkipped:
			skipped++
		}
	}
	terminalState := RolloutStateSucceeded
	terminalPauseCode := ""
	terminalEvent := "rollout_succeeded"
	if succeeded == 0 {
		terminalState = RolloutStateFailed
		terminalPauseCode = "all_nodes_skipped"
		terminalEvent = "rollout_failed"
	}
	if err := q.SetRolloutState(ctx, rollout.ID, terminalState, terminalPauseCode, at); err != nil {
		return nil, false, fmt.Errorf("complete rollout: %w", err)
	}
	if err := appendEvent(map[string]any{"event": "batch_completed", "batch": rollout.CurrentBatch}); err != nil {
		return nil, false, err
	}
	if err := appendEvent(map[string]any{"event": terminalEvent, "succeeded": succeeded, "skipped": skipped, "pause_code": terminalPauseCode}); err != nil {
		return nil, false, err
	}
	rollout.State = terminalState
	return nil, false, nil
}

// claimRolloutPendingNodes rechecks eligibility under the rollout lock and
// either claims a node for dispatch or records it as skipped with the
// exclusion reason. A skipped node pauses the rollout so the operator sees
// that the fleet no longer matches the approved request before the next
// batch starts.
func (s *Service) claimRolloutPendingNodes(ctx context.Context, q operationstore.Store, rollout *rolloutRow, current []*rolloutNodeRow, now time.Time, appendEvent func(map[string]any) error) ([]claimedRolloutNode, error) {
	at, err := value.FromTime(now)
	if err != nil {
		return nil, err
	}
	type eligibleClaim struct {
		observation rolloutNodeObservation
		digest      [sha256.Size]byte
	}
	eligible := make(map[uuid.UUID]eligibleClaim, len(current))
	// Preflight the complete dispatchable batch before taking any leases. If
	// one node changed eligibility, no peer from the approved batch may be
	// dispatched until an operator explicitly resumes the rollout.
	for _, node := range current {
		if node.State != RolloutNodePending || (node.DispatchLease.Valid && node.DispatchLease.Micros > at.Micros) {
			continue
		}
		observation, err := loadRolloutNodeObservation(ctx, q, node.NodeID, rollout.WorkspaceID)
		if err != nil {
			return nil, err
		}
		reason, digest, ok := s.rolloutNodeEligibility(now, observation, rollout.TargetVersion)
		if !ok {
			if _, err := pauseRolloutForSkippedNode(ctx, q, rollout, node, reason, now, appendEvent); err != nil {
				return nil, err
			}
			return nil, nil
		}
		eligible[node.NodeID] = eligibleClaim{observation: observation, digest: digest}
	}
	claimed := []claimedRolloutNode{}
	for _, node := range current {
		candidate, ok := eligible[node.NodeID]
		if !ok {
			continue
		}
		var nodeVersion int64
		if node.DispatchVersion > 0 {
			// A prior claim pinned the node version; a reclaim after a crash
			// reuses it so the deterministic idempotency key replays the exact
			// same operation instead of creating a duplicate upgrade.
			nodeVersion = node.DispatchVersion
		} else {
			nodeVersion, err = q.RolloutNodeVersion(ctx, node.NodeID)
			if err != nil {
				return nil, fmt.Errorf("read rollout node version: %w", err)
			}
		}
		attempt := node.DispatchAttempt
		if attempt < 1 {
			attempt = 1
		}
		lease, err := value.FromTime(now.Add(rolloutDispatchLease))
		if err != nil {
			return nil, err
		}
		changed, err := q.ClaimRolloutNode(ctx, rollout.ID, operationstore.RolloutNode{NodeID: node.NodeID, DispatchVersion: nodeVersion, DispatchAttempt: attempt}, lease, at)
		if err != nil {
			return nil, fmt.Errorf("claim rollout node: %w", err)
		}
		if !changed {
			continue
		}
		node.DispatchVersion = nodeVersion
		node.DispatchAttempt = attempt
		claimed = append(claimed, claimedRolloutNode{rolloutNodeRow: *node, Observation: candidate.observation, Digest: candidate.digest})
	}
	return claimed, nil
}

func pauseRolloutForSkippedNode(ctx context.Context, q operationstore.Store, rollout *rolloutRow, node *rolloutNodeRow, reason string, now time.Time, appendEvent func(map[string]any) error) (bool, error) {
	at, err := value.FromTime(now)
	if err != nil {
		return false, err
	}
	changed, err := q.SkipRolloutNode(ctx, rollout.ID, *node, reason, at)
	if err != nil {
		return false, fmt.Errorf("skip rollout node: %w", err)
	}
	if !changed {
		return false, nil
	}
	node.State = RolloutNodeSkipped
	node.FailureCode = reason
	rollout.State = RolloutStatePaused
	rollout.PauseCode = "node_skipped"
	if err := appendEvent(map[string]any{"event": "node_skipped", "batch": node.Batch, "node_id": node.NodeID.String(), "reason": reason}); err != nil {
		return false, err
	}
	if err := appendEvent(map[string]any{"event": "rollout_paused", "pause_code": "node_skipped", "batch": node.Batch, "node_id": node.NodeID.String(), "node_state": RolloutNodeSkipped}); err != nil {
		return false, err
	}
	return true, nil
}

// dispatchRolloutNode creates the reconciled single-node upgrade for one
// claimed node. The deterministic idempotency key and the pinned node
// version make a reclaim after a Controller crash replay the exact same
// operation instead of dispatching a duplicate upgrade.
func (s *Service) dispatchRolloutNode(ctx context.Context, rolloutID uuid.UUID, node claimedRolloutNode) (bool, error) {
	rollout, err := s.loadRollout(ctx, rolloutID)
	if err != nil {
		return false, fmt.Errorf("reload rollout for dispatch: %w", err)
	}
	if rollout.State != RolloutStateRunning {
		return true, nil
	}
	operation, _, err := s.CreateSynthetic(ctx, CreateRequest{
		NodeID:              node.NodeID,
		IdempotencyKey:      "agent-rollout:" + rolloutID.String() + ":" + node.NodeID.String() + ":" + strconv.Itoa(node.DispatchAttempt),
		ExpectedVersion:     node.DispatchVersion,
		Kind:                AgentUpgrade,
		TargetVersion:       rollout.TargetVersion,
		PackageSHA256:       node.Digest[:],
		Architecture:        node.Observation.Architecture,
		ApprovalID:          rollout.ApprovalID,
		ApprovalRequestHash: rollout.RequestHash,
		RolloutID:           rolloutID,
		Action:              "agent.upgrade",
		Reason:              rollout.Reason,
		TTL:                 rolloutUpgradeTTL,
		RequestID:           "agent-rollout:" + rolloutID.String() + ":" + node.NodeID.String(),
		Traceparent:         rolloutTraceparent(rolloutID, node.NodeID),
		ActorID:             rollout.CreatedBy.String(),
		ActorIdentityID:     rollout.CreatedBy,
		ActorSessionID:      rollout.ActorSession,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrStaleRevision):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "version_state_changed")
	case errors.Is(err, ErrCapabilityMissing):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "missing_capability")
	case errors.Is(err, ErrUpgradeActive):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "upgrade_in_progress")
	case errors.Is(err, ErrNodeUnavailable):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "node_unavailable")
	case errors.Is(err, approvals.ErrNotReady):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "approval_unavailable")
	case errors.Is(err, ErrIdempotencyConflict):
		return s.recordRolloutNodeSkipped(ctx, rolloutID, node.NodeID, "dispatch_conflict")
	case errors.Is(err, ErrRolloutState):
		return true, nil
	case errors.Is(err, ErrBacklogExceeded):
		// Global or node backlog pressure is transient: release the claim and
		// retry on a later pass instead of recording a failure.
		return false, s.releaseRolloutNodeClaim(ctx, rolloutID, node.NodeID)
	default:
		return false, fmt.Errorf("dispatch rollout node upgrade: %w", err)
	}
	at, err := value.FromTime(s.now())
	if err != nil {
		return false, err
	}
	operationID, err := uuid.Parse(operation.ID)
	if err != nil {
		return false, err
	}
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return false, fmt.Errorf("begin rollout dispatch record: %w", err)
	}
	defer rollbackRollout(tx)
	if err := q.AttachRolloutOperation(ctx, rolloutID, node.NodeID, operationID, node.Observation.AgentVersion, at); err != nil {
		return false, fmt.Errorf("record rollout dispatch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit rollout dispatch record: %w", err)
	}
	return false, nil
}

func (s *Service) recordRolloutNodeSkipped(ctx context.Context, rolloutID uuid.UUID, nodeID uuid.UUID, reason string) (bool, error) {
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return false, fmt.Errorf("begin rollout skip record: %w", err)
	}
	defer rollbackRollout(tx)
	rollout, err := q.Rollout(ctx, rolloutID, true)
	if err != nil {
		return false, fmt.Errorf("lock rollout skip record: %w", err)
	}
	node, err := q.LockRolloutNode(ctx, rolloutID, nodeID)
	if err != nil {
		return false, fmt.Errorf("lock skipped rollout node: %w", err)
	}
	now := s.now()
	events := []audit.ChainRecord{}
	appendEvent := func(summary map[string]any) error {
		eventID, eventErr := uuid.NewV7()
		if eventErr != nil {
			return eventErr
		}
		encoded, _ := json.Marshal(summary)
		events = append(events, audit.ChainRecord{
			EventID: eventID, WorkspaceID: rollout.WorkspaceID, ActorType: "controller", ActorID: "agent-rollout-orchestrator",
			Action: "agent.rollout", ResourceType: "agent_rollout", ResourceID: rollout.ID, AfterSummary: encoded, At: now,
		})
		return nil
	}
	paused, err := pauseRolloutForSkippedNode(ctx, q, &rollout, &node, reason, now, appendEvent)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if err := audit.AppendChainTx(ctx, tx, event); err != nil {
			return false, fmt.Errorf("append rollout skip audit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit rollout skip record: %w", err)
	}
	return paused, nil
}

func (s *Service) releaseRolloutNodeClaim(ctx context.Context, rolloutID uuid.UUID, nodeID uuid.UUID) error {
	at, err := value.FromTime(s.now())
	if err != nil {
		return err
	}
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return fmt.Errorf("begin rollout claim release: %w", err)
	}
	defer rollbackRollout(tx)
	if err := q.ReleaseRolloutClaim(ctx, rolloutID, nodeID, at); err != nil {
		return fmt.Errorf("release rollout node claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollout claim release: %w", err)
	}
	return nil
}

func (s *Service) beginRollout(ctx context.Context) (database.Tx, operationstore.Store, error) {
	tx, err := s.backend.Begin(ctx, database.ReadCommitted)
	if err != nil {
		return nil, nil, err
	}
	store, err := operationstore.FromTransaction(tx)
	if err != nil {
		rollbackRollout(tx)
		return nil, nil, err
	}
	return tx, store, nil
}

func rollbackRollout(tx database.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
