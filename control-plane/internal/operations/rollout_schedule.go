package operations

import (
	"context"

	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	tracecontext "github.com/GentleKingson/ocservia/control-plane/internal/platform/telemetry"
	"github.com/google/uuid"
)

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
	now := s.now().UTC()
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
		if err := q.SetRolloutState(ctx, rolloutID, "running", rollout.PauseCode, now); err != nil {
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
		changed, err := q.SetRolloutNodeOutcome(ctx, rollout.ID, node.NodeID, node.Outcome, failureCode, now)
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
		// Rollout admission fixes stop-on-failure to true; the stored policy
		// is part of the approved rollout request hash.
		pauseCode := "node_" + blocked.State
		if err := q.SetRolloutState(ctx, rollout.ID, "paused", pauseCode, now); err != nil {
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
		if err := q.SetRolloutBatch(ctx, rollout.ID, nextBatch, now); err != nil {
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
	return nil, false, completeRollout(ctx, q, rollout, nodes, now, appendEvent)
}

func completeRollout(ctx context.Context, q operationstore.Store, rollout *rolloutRow, nodes []rolloutNodeRow, at time.Time, appendEvent func(map[string]any) error) error {
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
		return fmt.Errorf("complete rollout: %w", err)
	}
	if err := appendEvent(map[string]any{"event": "batch_completed", "batch": rollout.CurrentBatch}); err != nil {
		return err
	}
	if err := appendEvent(map[string]any{"event": terminalEvent, "succeeded": succeeded, "skipped": skipped, "pause_code": terminalPauseCode}); err != nil {
		return err
	}
	rollout.State = terminalState
	return nil
}

// claimRolloutPendingNodes rechecks eligibility under the rollout lock and
// either claims a node for dispatch or records it as skipped with the
// exclusion reason. A skipped node pauses the rollout so the operator sees
// that the fleet no longer matches the approved request before the next
// batch starts.
func (s *Service) claimRolloutPendingNodes(ctx context.Context, q operationstore.Store, rollout *rolloutRow, current []*rolloutNodeRow, now time.Time, appendEvent func(map[string]any) error) ([]claimedRolloutNode, error) {
	type eligibleClaim struct {
		observation rolloutNodeObservation
		digest      [sha256.Size]byte
	}
	eligible := make(map[uuid.UUID]eligibleClaim, len(current))
	// Preflight the complete dispatchable batch before taking any leases. If
	// one node changed eligibility, no peer from the approved batch may be
	// dispatched until an operator explicitly resumes the rollout.
	for _, node := range current {
		if node.State != RolloutNodePending || (node.DispatchLease != nil && node.DispatchLease.After(now)) {
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
		var err error
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
		lease := now.Add(rolloutDispatchLease)
		changed, err := q.ClaimRolloutNode(ctx, rollout.ID, operationstore.RolloutNode{NodeID: node.NodeID, DispatchVersion: nodeVersion, DispatchAttempt: attempt}, lease, now)
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
	changed, err := q.SkipRolloutNode(ctx, rollout.ID, *node, reason, now)
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
	operation, _, err := s.Create(ctx, CreateRequest{
		NodeID:          node.NodeID,
		IdempotencyKey:  "agent-rollout:" + rolloutID.String() + ":" + node.NodeID.String() + ":" + strconv.Itoa(node.DispatchAttempt),
		ExpectedVersion: node.DispatchVersion,
		Action:          "agent.upgrade",
		Reason:          rollout.Reason,
		TTL:             rolloutUpgradeTTL,
		RequestID:       "agent-rollout:" + rolloutID.String() + ":" + node.NodeID.String(),
		Traceparent:     tracecontext.NewTraceparent(),
		ActorID:         rollout.CreatedBy.String(),
		ActorIdentityID: rollout.CreatedBy,
		ActorSessionID:  rollout.ActorSession,
		Payload: AgentUpgradePayload{
			TargetVersion: rollout.TargetVersion,
			PackageSHA256: node.Digest[:],
			Architecture:  node.Observation.Architecture,
			RolloutID:     rolloutID,
		},
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
	now := s.now().UTC()
	operationID, err := uuid.Parse(operation.ID)
	if err != nil {
		return false, err
	}
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return false, fmt.Errorf("begin rollout dispatch record: %w", err)
	}
	defer rollbackRollout(tx)
	if err := q.AttachRolloutOperation(ctx, rolloutID, node.NodeID, operationID, node.Observation.AgentVersion, now); err != nil {
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
	tx, q, err := s.beginRollout(ctx)
	if err != nil {
		return fmt.Errorf("begin rollout claim release: %w", err)
	}
	defer rollbackRollout(tx)
	if err := q.ReleaseRolloutClaim(ctx, rolloutID, nodeID, s.now().UTC()); err != nil {
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
