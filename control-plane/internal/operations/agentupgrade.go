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
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pendingUpgrade is one scheduled single-node agent upgrade together with the
// durable Controller-side evidence needed to reconcile it.
type pendingUpgrade struct {
	OperationID     uuid.UUID
	WorkspaceID     uuid.UUID
	NodeID          uuid.UUID
	CommandID       uuid.UUID
	TargetVersion   string
	FromVersion     string
	State           string
	ScheduledAt     time.Time
	CreatedAt       time.Time
	OperationState  string
	NodeStatus      string
	ObservedVersion string
	ObservedAt      time.Time
	LastHeartbeatAt time.Time
	DurableState    string
	DurableDetail   string
}

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
	rows, err := s.pool.Query(ctx, `
		SELECT u.operation_id,u.workspace_id,u.node_id,op.command_id,u.target_version,u.from_version,u.state,COALESCE(u.scheduled_at,u.created_at),u.created_at,
		       op.state,n.status,COALESCE(snap.agent_version,''),COALESCE(snap.observed_at,to_timestamp(0)),COALESCE(snap.last_heartbeat_at,to_timestamp(0)),
		       COALESCE(r.state,''),COALESCE(r.detail,'')
		FROM agent_upgrade_operations u
		JOIN operations op ON op.id=u.operation_id
		JOIN nodes n ON n.id=u.node_id
		LEFT JOIN node_observed_snapshots snap ON snap.node_id=u.node_id
		LEFT JOIN node_agent_upgrade_results r ON r.operation_id=u.operation_id
		WHERE u.completed_at IS NULL
		  AND u.state IN ('queued','accepted','running','unknown')
		LIMIT $1`, reconciliationBatchLimit)
	if err != nil {
		return fmt.Errorf("scan pending agent upgrades: %w", err)
	}
	defer rows.Close()
	pending := make([]pendingUpgrade, 0, reconciliationBatchLimit)
	for rows.Next() {
		var upgrade pendingUpgrade
		if err := rows.Scan(&upgrade.OperationID, &upgrade.WorkspaceID, &upgrade.NodeID, &upgrade.CommandID, &upgrade.TargetVersion, &upgrade.FromVersion, &upgrade.State, &upgrade.ScheduledAt, &upgrade.CreatedAt, &upgrade.OperationState, &upgrade.NodeStatus, &upgrade.ObservedVersion, &upgrade.ObservedAt, &upgrade.LastHeartbeatAt, &upgrade.DurableState, &upgrade.DurableDetail); err != nil {
			return fmt.Errorf("read pending agent upgrade: %w", err)
		}
		pending = append(pending, upgrade)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending agent upgrades: %w", err)
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
	online := upgrade.NodeStatus == "active" && now.Sub(upgrade.LastHeartbeatAt) <= telemetry.OfflineAfter
	base := upgrade.ScheduledAt
	switch {
	case upgrade.DurableState == "failed":
		return agentUpgradeDecision{State: "failed", Terminal: true, Reason: "durable_local_failure"}, true
	case upgrade.DurableState == "rolled_back":
		return agentUpgradeDecision{State: "rolled_back", Terminal: true, Reason: "durable_local_rollback"}, true
	case upgrade.DurableState == "succeeded" && online && upgrade.ObservedVersion == upgrade.TargetVersion:
		return agentUpgradeDecision{State: "succeeded", Terminal: true, Reason: "durable_success_and_target_version_observed"}, true
	case now.After(base.Add(s.agentUpgradeReconcileTime)):
		return agentUpgradeDecision{State: "unknown", Terminal: true, Reason: "reconciliation_deadline_exceeded"}, true
	case upgrade.State == "accepted" && online && upgrade.ObservedAt.After(upgrade.ScheduledAt):
		return agentUpgradeDecision{State: "running", Terminal: false, Reason: "node_reconnected_verifying_target_version"}, true
	}
	return agentUpgradeDecision{}, false
}

func (s *Service) applyAgentUpgradeTerminal(ctx context.Context, upgrade pendingUpgrade, decision agentUpgradeDecision) error {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin agent upgrade reconciliation: %w", err)
	}
	defer rollback(tx)
	commandState := decision.State
	if commandState == "unknown" {
		// No terminal command result will ever arrive; expire the delivery
		// lifecycle so the generic recovery loops stop re-dispatching.
		commandState = "expired"
	}
	// A generic-engine terminal (expired or superseded) already closed the
	// operation, command, and outbox; only the projection lags behind.
	genericTerminal := upgrade.OperationState == "expired" || upgrade.OperationState == "superseded"
	if !genericTerminal && decision.State != upgrade.OperationState {
		tag, err := tx.Exec(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=$3,completed_at=GREATEST(COALESCE(completed_at,$3),$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, upgrade.OperationID, decision.State, now)
		if err != nil {
			return fmt.Errorf("apply agent upgrade operation outcome: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE commands SET state=$2,updated_at=$3 WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, upgrade.CommandID, commandState, now); err != nil {
			return fmt.Errorf("apply agent upgrade command outcome: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, upgrade.CommandID, now); err != nil {
			return fmt.Errorf("complete agent upgrade outbox: %w", err)
		}
	}
	projectionTag, err := tx.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,completed_at=$3,updated_at=$3 WHERE operation_id=$1 AND completed_at IS NULL AND state IN ('queued','accepted','running','unknown')`, upgrade.OperationID, decision.State, now)
	if err != nil {
		return fmt.Errorf("apply agent upgrade projection outcome: %w", err)
	}
	if projectionTag.RowsAffected() == 0 {
		return nil
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,$3,$4)`, eventID, upgrade.OperationID, decision.State, now); err != nil {
		return fmt.Errorf("append agent upgrade outcome event: %w", err)
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
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
		EventID: auditID, WorkspaceID: upgrade.WorkspaceID, ActorType: "controller", ActorID: "agent-upgrade-reconciler",
		Action: "agent.upgrade", ResourceType: "operation", ResourceID: upgrade.OperationID,
		NodeID: &upgrade.NodeID, CommandID: &upgrade.CommandID, Result: auditResult,
		AfterSummary: summary, At: now,
	}); err != nil {
		return fmt.Errorf("append agent upgrade outcome audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent upgrade reconciliation: %w", err)
	}
	return nil
}

func (s *Service) applyAgentUpgradeProgress(ctx context.Context, upgrade pendingUpgrade, decision agentUpgradeDecision) error {
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin agent upgrade progress: %w", err)
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=$3 WHERE id=$1 AND state='accepted'`, upgrade.OperationID, decision.State, now)
	if err != nil {
		return fmt.Errorf("apply agent upgrade progress: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,updated_at=$3 WHERE operation_id=$1 AND state='accepted'`, upgrade.OperationID, decision.State, now); err != nil {
		return fmt.Errorf("apply agent upgrade projection progress: %w", err)
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,$3,$4)`, eventID, upgrade.OperationID, decision.State, now); err != nil {
		return fmt.Errorf("append agent upgrade progress event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent upgrade progress: %w", err)
	}
	return nil
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
	ID            string                  `json:"id"`
	WorkspaceID   string                  `json:"workspace_id"`
	TargetVersion string                  `json:"target_version"`
	State         string                  `json:"state"`
	BatchSize     int                     `json:"batch_size"`
	StopOnFailure bool                    `json:"stop_on_failure"`
	Reason        string                  `json:"reason"`
	ApprovalID    string                  `json:"approval_id"`
	CreatedBy     string                  `json:"created_by"`
	CurrentBatch  int                     `json:"current_batch"`
	PauseCode     string                  `json:"pause_code,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	Nodes         []AgentRolloutNode      `json:"nodes,omitempty"`
	Excluded      []AgentRolloutExclusion `json:"excluded,omitempty"`
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
type rolloutRow struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	TargetVersion string
	State         string
	BatchSize     int
	StopOnFailure bool
	Reason        string
	ApprovalID    uuid.UUID
	RequestHash   []byte
	CreatedBy     uuid.UUID
	ActorSession  uuid.UUID
	CurrentBatch  int
	PauseCode     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type rolloutNodeRow struct {
	NodeID          uuid.UUID
	Ordinal         int
	Batch           int
	State           string
	OperationID     uuid.UUID
	FromVersion     string
	FailureCode     string
	DispatchVersion int64
	DispatchAttempt int
	DispatchLease   time.Time
}

type claimedRolloutNode struct {
	rolloutNodeRow
	Observation rolloutNodeObservation
	Digest      [sha256.Size]byte
}

// rolloutNodeObservation is the durable evidence eligibility derives from.
type rolloutNodeObservation struct {
	NodeID          uuid.UUID
	Status          string
	Architecture    string
	AgentVersion    string
	LastHeartbeatAt time.Time
	CapabilityOK    bool
	UpgradeActive   bool
}

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
	case now.Sub(node.LastHeartbeatAt) > telemetry.OfflineAfter:
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

func scanRolloutRow(row pgx.Row) (rolloutRow, error) {
	var rollout rolloutRow
	var id, workspace, approval, createdBy, actorSession uuid.UUID
	var requestHash []byte
	err := row.Scan(&id, &workspace, &rollout.TargetVersion, &rollout.State, &rollout.BatchSize, &rollout.StopOnFailure,
		&rollout.Reason, &approval, &requestHash, &createdBy, &actorSession, &rollout.CurrentBatch, &rollout.PauseCode,
		&rollout.CreatedAt, &rollout.UpdatedAt)
	if err != nil {
		return rolloutRow{}, err
	}
	rollout.ID, rollout.WorkspaceID, rollout.ApprovalID = id, workspace, approval
	rollout.RequestHash, rollout.CreatedBy, rollout.ActorSession = requestHash, createdBy, actorSession
	return rollout, nil
}

func rolloutFromRow(rollout rolloutRow, nodes []AgentRolloutNode) AgentRollout {
	return AgentRollout{
		ID: rollout.ID.String(), WorkspaceID: rollout.WorkspaceID.String(), TargetVersion: rollout.TargetVersion,
		State: rollout.State, BatchSize: rollout.BatchSize, StopOnFailure: rollout.StopOnFailure,
		Reason: rollout.Reason, ApprovalID: rollout.ApprovalID.String(), CreatedBy: rollout.CreatedBy.String(),
		CurrentBatch: rollout.CurrentBatch, PauseCode: rollout.PauseCode,
		CreatedAt: rollout.CreatedAt, UpdatedAt: rollout.UpdatedAt, Nodes: nodes,
	}
}

// rolloutTraceparent derives a stable, valid traceparent from the rollout
// and node identities so every dispatch attempt of the same node shares one
// trace.
func rolloutTraceparent(rolloutID, nodeID uuid.UUID) string {
	digest := sha256.Sum256(append(rolloutID[:], nodeID[:]...))
	return fmt.Sprintf("00-%032x-%016x-01", digest[:16], digest[16:24])
}

// rolloutQueryer abstracts the pool and transaction query surface so the
// rollout helpers run inside the caller's transaction or standalone.
type rolloutQueryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const rolloutHeaderSelect = "SELECT id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,current_batch,pause_code,created_at,updated_at" +
	" FROM agent_rollouts"

func loadRolloutNodeObservation(ctx context.Context, q rolloutQueryer, nodeID, workspaceID uuid.UUID) (rolloutNodeObservation, error) {
	var node rolloutNodeObservation
	node.NodeID = nodeID
	err := q.QueryRow(ctx, "SELECT n.status,COALESCE(o.architecture,''),COALESCE(o.agent_version,''),COALESCE(o.last_heartbeat_at,to_timestamp(0)),"+
		"EXISTS(SELECT 1 FROM node_capabilities c WHERE c.node_id=n.id AND c.capability='ocserv.agent.upgrade.v2' AND c.approved=true),"+
		"EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.completed_at IS NULL AND u.state IN ('queued','accepted','running','unknown'))"+
		" FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1 AND n.workspace_id=$2", nodeID, workspaceID).Scan(
		&node.Status, &node.Architecture, &node.AgentVersion, &node.LastHeartbeatAt, &node.CapabilityOK, &node.UpgradeActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return node, ErrRolloutInvalid
	}
	return node, err
}

func readRolloutByIdempotencyKey(ctx context.Context, q rolloutQueryer, workspaceID uuid.UUID, key string) (uuid.UUID, bool, error) {
	var id, workspace uuid.UUID
	var state string
	err := q.QueryRow(ctx, "SELECT id,workspace_id,state FROM agent_rollouts WHERE workspace_id=$1 AND idempotency_key=$2", workspaceID, key).Scan(&id, &workspace, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read agent rollout idempotency: %w", err)
	}
	return id, true, nil
}

func rolloutRequestHashByID(ctx context.Context, q rolloutQueryer, rolloutID uuid.UUID) (string, error) {
	var storedHash []byte
	if err := q.QueryRow(ctx, "SELECT request_hash FROM agent_rollouts WHERE id=$1", rolloutID).Scan(&storedHash); err != nil {
		return "", fmt.Errorf("read agent rollout request hash: %w", err)
	}
	return string(storedHash), nil
}

// GetAgentRollout reads the rollout header and its nodes in stable ordinal order.
func (s *Service) GetAgentRollout(ctx context.Context, rolloutID uuid.UUID) (AgentRollout, error) {
	rollout, err := scanRolloutRow(s.pool.QueryRow(ctx, rolloutHeaderSelect+" WHERE id=$1", rolloutID))
	if err != nil {
		return AgentRollout{}, err
	}
	nodes, err := s.rolloutNodes(ctx, s.pool, rollout.ID)
	if err != nil {
		return AgentRollout{}, err
	}
	return rolloutFromRow(rollout, nodes), nil
}

// ListAgentRollouts returns the workspace's most recent rollouts with their
// node projections so the fleet view can show bounded progress summaries.
func (s *Service) ListAgentRollouts(ctx context.Context, workspaceID uuid.UUID, limit int) ([]AgentRollout, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, rolloutHeaderSelect+" WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT $2", workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent rollouts: %w", err)
	}
	defer rows.Close()
	rollouts := []AgentRollout{}
	for rows.Next() {
		rollout, scanErr := scanRolloutRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		rollouts = append(rollouts, rolloutFromRow(rollout, nil))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range rollouts {
		rolloutID, parseErr := uuid.Parse(rollouts[index].ID)
		if parseErr != nil {
			return nil, parseErr
		}
		nodes, err := s.rolloutNodes(ctx, s.pool, rolloutID)
		if err != nil {
			return nil, err
		}
		rollouts[index].Nodes = nodes
	}
	return rollouts, nil
}

// RolloutWorkspace resolves the owning workspace of a rollout for
// workspace-scoped request authorization.
func RolloutWorkspace(ctx context.Context, pool *pgxpool.Pool, rolloutID uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := pool.QueryRow(ctx, "SELECT workspace_id FROM agent_rollouts WHERE id=$1", rolloutID).Scan(&workspaceID)
	return workspaceID, err
}

func (s *Service) rolloutNodes(ctx context.Context, q rolloutQueryer, rolloutID uuid.UUID) ([]AgentRolloutNode, error) {
	rows, err := q.Query(ctx, "SELECT node_id::text,ordinal,batch,state,COALESCE(operation_id::text,''),from_version,failure_code"+
		" FROM agent_rollout_nodes WHERE rollout_id=$1 ORDER BY ordinal", rolloutID)
	if err != nil {
		return nil, fmt.Errorf("read agent rollout nodes: %w", err)
	}
	defer rows.Close()
	nodes := []AgentRolloutNode{}
	for rows.Next() {
		var node AgentRolloutNode
		if err := rows.Scan(&node.NodeID, &node.Ordinal, &node.Batch, &node.State, &node.OperationID, &node.FromVersion, &node.FailureCode); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRollout{}, false, fmt.Errorf("begin agent rollout creation: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	if existingID, found, err := readRolloutByIdempotencyKey(ctx, q, request.WorkspaceID, request.IdempotencyKey); err != nil {
		return AgentRollout{}, false, err
	} else if found {
		if existingHash, hashErr := rolloutRequestHashByID(ctx, q, existingID); hashErr != nil || string(existingHash) != string(requestHash) {
			return AgentRollout{}, false, ErrIdempotencyConflict
		}
		rollout, loadErr := s.GetAgentRollout(ctx, existingID)
		if loadErr != nil {
			return AgentRollout{}, false, loadErr
		}
		return rollout, true, nil
	}
	// The approval row carries the rollout identity: the requester pinned the
	// exact rollout request at approval time and consumption binds this
	// rollout to it. A reused, mismatched, or unapproved approval fails
	// closed.
	var approvedWorkspace, approvedResource, approvedRequester uuid.UUID
	var approvedAction, approvedType, approvedStatus string
	var approvedHash []byte
	var approvedExpiry time.Time
	err = q.QueryRow(ctx, "SELECT workspace_id,resource_id,requester_id,action,resource_type,status,expires_at,request_hash"+
		" FROM approval_requests WHERE id=$1 FOR UPDATE", request.ApprovalID).Scan(
		&approvedWorkspace, &approvedResource, &approvedRequester, &approvedAction, &approvedType, &approvedStatus, &approvedExpiry, &approvedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRollout{}, false, approvals.ErrNotReady
	}
	if err != nil {
		return AgentRollout{}, false, fmt.Errorf("lock rollout approval: %w", err)
	}
	if approvedWorkspace != request.WorkspaceID || approvedRequester != request.ActorIdentityID ||
		approvedAction != "agent.rollout" || approvedType != "batch_operation" ||
		approvedStatus != "approved" || !approvedExpiry.After(now) || approvedResource.Version() != 7 ||
		len(approvedHash) != sha256.Size || string(approvedHash) != string(requestHash) {
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
		node, loadErr := loadRolloutNodeObservation(ctx, q, nodeID, request.WorkspaceID)
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
	auditID, err := uuid.NewV7()
	if err != nil {
		return AgentRollout{}, false, err
	}
	if _, err := q.Exec(ctx, "INSERT INTO agent_rollouts(id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,current_batch,pause_code,idempotency_key,created_at,updated_at)"+
		" VALUES($1,$2,$3,'queued',$4,true,$5,$6,$7,$8,$9,0,'',$10,$11,$11)",
		rolloutID, request.WorkspaceID, target, request.BatchSize, reason, request.ApprovalID, requestHash[:], request.ActorIdentityID, request.ActorSessionID, request.IdempotencyKey, now); err != nil {
		return AgentRollout{}, false, fmt.Errorf("insert agent rollout: %w", err)
	}
	for ordinal, node := range eligible {
		if _, err := q.Exec(ctx, "INSERT INTO agent_rollout_nodes(rollout_id,node_id,ordinal,batch,state,from_version,updated_at)"+
			" VALUES($1,$2,$3,$4,'pending','',$5)", rolloutID, node.nodeID, ordinal, rolloutBatchForOrdinal(ordinal, request.BatchSize), now); err != nil {
			return AgentRollout{}, false, fmt.Errorf("insert agent rollout node: %w", err)
		}
	}
	summary, _ := json.Marshal(map[string]any{
		"target_version": target, "batch_size": request.BatchSize, "stop_on_failure": true,
		"node_count": len(eligible), "node_ids": nodeIDStrings(sorted), "excluded": exclusions,
	})
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
		EventID: auditID, WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID,
		SessionID: &request.ActorSessionID, Action: "agent.rollout", ResourceType: "agent_rollout",
		ResourceID: rolloutID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID,
		TraceID: traceID(request.Traceparent), Reason: reason, Result: "intent", AfterSummary: summary, At: now,
	}); err != nil {
		return AgentRollout{}, false, fmt.Errorf("append agent rollout audit: %w", err)
	}
	if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorIdentityID, "agent.rollout", "batch_operation", rolloutID, requestHash[:]); err != nil {
		return AgentRollout{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentRollout{}, false, fmt.Errorf("commit agent rollout creation: %w", err)
	}
	rollout, err := s.GetAgentRollout(ctx, rolloutID)
	if err != nil {
		return AgentRollout{}, false, err
	}
	rollout.Excluded = exclusions
	return rollout, false, nil
}

// ResumeAgentRollout records an explicit operator decision to continue a
// paused rollout. Succeeded nodes are never redispatched; failed, unknown,
// and rolled-back nodes of the current batch are requeued for a fresh
// eligibility check and upgrade attempt, while skipped nodes keep their
// recorded exclusion reason.
func (s *Service) ResumeAgentRollout(ctx context.Context, rolloutID uuid.UUID, actorID string, actorIdentityID, actorSessionID uuid.UUID, requestID, traceparent string) (AgentRollout, error) {
	if actorIdentityID == uuid.Nil || actorSessionID == uuid.Nil {
		return AgentRollout{}, ErrRolloutInvalid
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRollout{}, fmt.Errorf("begin agent rollout resume: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	rollout, err := scanRolloutRow(q.QueryRow(ctx, rolloutHeaderSelect+" WHERE id=$1 FOR UPDATE", rolloutID))
	if err != nil {
		return AgentRollout{}, err
	}
	if rollout.State != RolloutStatePaused {
		return AgentRollout{}, ErrRolloutState
	}
	tag, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET state='pending',failure_code='',dispatch_node_version=NULL,dispatch_attempt=dispatch_attempt+1,dispatch_lease_until=NULL,updated_at=$3"+
		" WHERE rollout_id=$1 AND batch=$2 AND state IN ('failed','rolled_back','unknown')", rolloutID, rollout.CurrentBatch, now)
	if err != nil {
		return AgentRollout{}, fmt.Errorf("requeue failed rollout nodes: %w", err)
	}
	requeued := tag.RowsAffected()
	if _, err := q.Exec(ctx, "UPDATE agent_rollouts SET state='running',pause_code='',updated_at=$2 WHERE id=$1 AND state='paused'", rolloutID, now); err != nil {
		return AgentRollout{}, fmt.Errorf("resume agent rollout: %w", err)
	}
	summary, _ := json.Marshal(map[string]any{"event": "rollout_resumed", "current_batch": rollout.CurrentBatch, "requeued_nodes": requeued})
	auditID, err := uuid.NewV7()
	if err != nil {
		return AgentRollout{}, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{
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
	rows, err := s.pool.Query(ctx, "SELECT id FROM agent_rollouts WHERE state IN ('queued','running') ORDER BY created_at LIMIT $1", rolloutAdvanceLimit)
	if err != nil {
		return fmt.Errorf("list advancing rollouts: %w", err)
	}
	rolloutIDs := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		rolloutIDs = append(rolloutIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
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
			if err := s.dispatchRolloutNode(ctx, rolloutID, node); err != nil {
				return err
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin rollout advance: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	rollout, err := scanRolloutRow(q.QueryRow(ctx, rolloutHeaderSelect+" WHERE id=$1 FOR UPDATE", rolloutID))
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
		if _, err := q.Exec(ctx, "UPDATE agent_rollouts SET state='running',updated_at=$2 WHERE id=$1", rolloutID, now); err != nil {
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
		if err := audit.AppendChain(ctx, tx, event); err != nil {
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
func (s *Service) rollUpTerminalRolloutNodes(ctx context.Context, q rolloutQueryer, rollout *rolloutRow, now time.Time, appendEvent func(map[string]any) error) error {
	rows, err := q.Query(ctx, "SELECT rn.node_id,rn.ordinal,rn.batch,COALESCE(rn.operation_id::text,''),u.state"+
		" FROM agent_rollout_nodes rn"+
		" JOIN operations op ON op.id=rn.operation_id"+
		" JOIN agent_upgrade_operations u ON u.operation_id=rn.operation_id"+
		" WHERE rn.rollout_id=$1 AND rn.state='running' AND u.completed_at IS NOT NULL", rollout.ID)
	if err != nil {
		return fmt.Errorf("read terminal rollout nodes: %w", err)
	}
	type terminalNode struct {
		nodeID      uuid.UUID
		ordinal     int
		batch       int
		operationID string
		outcome     string
	}
	terminal := []terminalNode{}
	for rows.Next() {
		var node terminalNode
		var operationID, upgradeState string
		if err := rows.Scan(&node.nodeID, &node.ordinal, &node.batch, &operationID, &upgradeState); err != nil {
			rows.Close()
			return err
		}
		node.operationID = operationID
		node.outcome = upgradeState
		if !slices.Contains([]string{"succeeded", "failed", "rolled_back", "unknown"}, node.outcome) {
			// The operation lifecycle closed without a reconciled upgrade
			// outcome (expired command); the upgrade outcome is unknown.
			node.outcome = RolloutNodeUnknown
		}
		terminal = append(terminal, node)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, node := range terminal {
		failureCode := ""
		switch node.outcome {
		case RolloutNodeFailed:
			failureCode = "upgrade_failed"
		case RolloutNodeRolledBack:
			failureCode = "upgrade_rolled_back"
		case RolloutNodeUnknown:
			failureCode = "outcome_unknown"
		}
		tag, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET state=$3,failure_code=$4,updated_at=$5"+
			" WHERE rollout_id=$1 AND node_id=$2 AND state='running'", rollout.ID, node.nodeID, node.outcome, failureCode, now)
		if err != nil {
			return fmt.Errorf("roll up rollout node outcome: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if node.outcome != RolloutNodeSucceeded {
			if err := appendEvent(map[string]any{
				"event": "node_" + node.outcome, "batch": node.batch, "node_id": node.nodeID.String(),
				"operation_id": node.operationID, "failure_code": failureCode,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluateRolloutBatch enforces stop-on-first-failure/unknown, advances the
// batch pointer, and claims the dispatchable pending nodes of the current
// batch. Skipped nodes never block advancement but never count as upgrades.
// The second return reports whether the batch pointer advanced.
func (s *Service) evaluateRolloutBatch(ctx context.Context, q rolloutQueryer, rollout *rolloutRow, now time.Time, appendEvent func(map[string]any) error) ([]claimedRolloutNode, bool, error) {
	nodes, err := readRolloutNodesLocked(ctx, q, rollout.ID)
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
		if _, err := q.Exec(ctx, "UPDATE agent_rollouts SET state='paused',pause_code=$2,updated_at=$3 WHERE id=$1", rollout.ID, pauseCode, now); err != nil {
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
		if _, err := q.Exec(ctx, "UPDATE agent_rollouts SET current_batch=$2,updated_at=$3 WHERE id=$1", rollout.ID, nextBatch, now); err != nil {
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
	if _, err := q.Exec(ctx, "UPDATE agent_rollouts SET state=$2,pause_code=$3,updated_at=$4 WHERE id=$1", rollout.ID, terminalState, terminalPauseCode, now); err != nil {
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

func readRolloutNodesLocked(ctx context.Context, q rolloutQueryer, rolloutID uuid.UUID) ([]rolloutNodeRow, error) {
	rows, err := q.Query(ctx, "SELECT node_id,ordinal,batch,state,operation_id,from_version,failure_code,COALESCE(dispatch_node_version,0),dispatch_attempt,COALESCE(dispatch_lease_until,to_timestamp(0))"+
		" FROM agent_rollout_nodes WHERE rollout_id=$1 ORDER BY ordinal", rolloutID)
	if err != nil {
		return nil, fmt.Errorf("read rollout nodes: %w", err)
	}
	defer rows.Close()
	nodes := []rolloutNodeRow{}
	for rows.Next() {
		var node rolloutNodeRow
		var operationID *uuid.UUID
		if err := rows.Scan(&node.NodeID, &node.Ordinal, &node.Batch, &node.State, &operationID, &node.FromVersion, &node.FailureCode, &node.DispatchVersion, &node.DispatchAttempt, &node.DispatchLease); err != nil {
			return nil, err
		}
		if operationID != nil {
			node.OperationID = *operationID
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// claimRolloutPendingNodes rechecks eligibility under the rollout lock and
// either claims a node for dispatch or records it as skipped with the
// exclusion reason. A skipped node pauses the rollout so the operator sees
// that the fleet no longer matches the approved request before the next
// batch starts.
func (s *Service) claimRolloutPendingNodes(ctx context.Context, q rolloutQueryer, rollout *rolloutRow, current []*rolloutNodeRow, now time.Time, appendEvent func(map[string]any) error) ([]claimedRolloutNode, error) {
	claimed := []claimedRolloutNode{}
	for _, node := range current {
		if node.State != RolloutNodePending || node.DispatchLease.After(now) {
			continue
		}
		observation, err := loadRolloutNodeObservation(ctx, q, node.NodeID, rollout.WorkspaceID)
		if err != nil {
			return nil, err
		}
		reason, digest, eligible := s.rolloutNodeEligibility(now, observation, rollout.TargetVersion)
		if !eligible {
			if _, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET state='skipped',failure_code=$3,dispatch_lease_until=NULL,updated_at=$4"+
				" WHERE rollout_id=$1 AND node_id=$2 AND state='pending'", rollout.ID, node.NodeID, reason, now); err != nil {
				return nil, fmt.Errorf("skip rollout node: %w", err)
			}
			node.State = RolloutNodeSkipped
			node.FailureCode = reason
			if err := appendEvent(map[string]any{"event": "node_skipped", "batch": node.Batch, "node_id": node.NodeID.String(), "reason": reason}); err != nil {
				return nil, err
			}
			continue
		}
		var nodeVersion int64
		if node.DispatchVersion > 0 {
			// A prior claim pinned the node version; a reclaim after a crash
			// reuses it so the deterministic idempotency key replays the exact
			// same operation instead of creating a duplicate upgrade.
			nodeVersion = node.DispatchVersion
		} else if err := q.QueryRow(ctx, "SELECT version FROM nodes WHERE id=$1", node.NodeID).Scan(&nodeVersion); err != nil {
			return nil, fmt.Errorf("read rollout node version: %w", err)
		}
		attempt := node.DispatchAttempt
		if attempt < 1 {
			attempt = 1
		}
		tag, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET dispatch_node_version=$3,dispatch_attempt=$4,dispatch_lease_until=$5,updated_at=$6"+
			" WHERE rollout_id=$1 AND node_id=$2 AND state='pending' AND (dispatch_lease_until IS NULL OR dispatch_lease_until<=$7)",
			rollout.ID, node.NodeID, nodeVersion, attempt, now.Add(rolloutDispatchLease), now, now)
		if err != nil {
			return nil, fmt.Errorf("claim rollout node: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		node.DispatchVersion = nodeVersion
		node.DispatchAttempt = attempt
		claimed = append(claimed, claimedRolloutNode{rolloutNodeRow: *node, Observation: observation, Digest: digest})
	}
	return claimed, nil
}

// dispatchRolloutNode creates the reconciled single-node upgrade for one
// claimed node. The deterministic idempotency key and the pinned node
// version make a reclaim after a Controller crash replay the exact same
// operation instead of dispatching a duplicate upgrade.
func (s *Service) dispatchRolloutNode(ctx context.Context, rolloutID uuid.UUID, node claimedRolloutNode) error {
	rollout, err := scanRolloutRow(s.pool.QueryRow(ctx, rolloutHeaderSelect+" WHERE id=$1", rolloutID))
	if err != nil {
		return fmt.Errorf("reload rollout for dispatch: %w", err)
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
	case errors.Is(err, ErrBacklogExceeded):
		// Global or node backlog pressure is transient: release the claim and
		// retry on a later pass instead of recording a failure.
		return s.releaseRolloutNodeClaim(ctx, rolloutID, node.NodeID)
	default:
		return fmt.Errorf("dispatch rollout node upgrade: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollout dispatch record: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	if _, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET state='running',operation_id=$3,from_version=$4,dispatch_lease_until=NULL,updated_at=$5"+
		" WHERE rollout_id=$1 AND node_id=$2 AND state='pending'", rolloutID, node.NodeID, operation.ID, node.Observation.AgentVersion, s.now()); err != nil {
		return fmt.Errorf("record rollout dispatch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollout dispatch record: %w", err)
	}
	return nil
}

func (s *Service) recordRolloutNodeSkipped(ctx context.Context, rolloutID uuid.UUID, nodeID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollout skip record: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	if _, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET state='skipped',failure_code=$3,dispatch_lease_until=NULL,updated_at=$4"+
		" WHERE rollout_id=$1 AND node_id=$2 AND state='pending'", rolloutID, nodeID, reason, s.now()); err != nil {
		return fmt.Errorf("record rollout node skip: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollout skip record: %w", err)
	}
	return nil
}

func (s *Service) releaseRolloutNodeClaim(ctx context.Context, rolloutID uuid.UUID, nodeID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin rollout claim release: %w", err)
	}
	defer rollback(tx)
	q := rolloutQueryer(tx)
	if _, err := q.Exec(ctx, "UPDATE agent_rollout_nodes SET dispatch_lease_until=NULL,updated_at=$3"+
		" WHERE rollout_id=$1 AND node_id=$2", rolloutID, nodeID, s.now()); err != nil {
		return fmt.Errorf("release rollout node claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rollout claim release: %w", err)
	}
	return nil
}
