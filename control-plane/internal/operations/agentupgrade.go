package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
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
