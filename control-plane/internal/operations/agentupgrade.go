package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
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
