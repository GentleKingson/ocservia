package operations

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// upgradeReconciliationFixture provisions one workspace, node, and observed
// snapshot so reconciliation scenarios only mutate evidence rows.
func upgradeReconciliationFixture(t *testing.T) (*Service, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	service, pool, workspaceID, nodeID := integrationService(t)
	// The shared cleanup deletes operations, which agent_upgrade_operations
	// restricts: drop the family rows first.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, statement := range []string{
			`DELETE FROM node_agent_upgrade_results WHERE node_id IN(SELECT id FROM nodes WHERE workspace_id=$1)`,
			`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`,
		} {
			_, _ = pool.Exec(ctx, statement, workspaceID)
		}
	})
	// Keep the reconciliation deadline far away unless a scenario backdates
	// the scheduling acknowledgement on purpose.
	service.SetAgentUpgradeReconcileTimeout(2 * time.Hour)
	observeUpgradeNode(t, pool, nodeID, "1.2.0", 0)
	return service, pool, workspaceID, nodeID
}

func observeUpgradeNode(t *testing.T, pool *pgxpool.Pool, nodeID uuid.UUID, agentVersion string, offlineFor time.Duration) {
	t.Helper()
	observedAt := time.Now().UTC()
	heartbeat := observedAt.Add(-offlineFor)
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at)
		VALUES($1,$2,'upgrade-fixture-boot',$3,$4,'1.2.3','Debian GNU/Linux 12','amd64','{}','{}','{}',$5)
		ON CONFLICT(node_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,agent_version=EXCLUDED.agent_version,last_heartbeat_at=EXCLUDED.last_heartbeat_at`,
		nodeID, observedAt, uuid.Must(uuid.NewV7()), agentVersion, heartbeat); err != nil {
		t.Fatal(err)
	}
}

// createScheduledUpgrade drives the API-equivalent creation path and returns
// the operation ID of a queued upgrade targeting 2.0.0 from 1.2.0.
func createScheduledUpgrade(t *testing.T, service *Service, pool *pgxpool.Pool, workspaceID, nodeID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',true) ON CONFLICT(node_id,capability) DO UPDATE SET approved=true`, nodeID); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{NodeID: nodeID, IdempotencyKey: key, ExpectedVersion: 1, Kind: AgentUpgrade, ActorID: "operator", Action: "agent.upgrade", Reason: "integration test", TargetVersion: "2.0.0", PackageSHA256: bytes.Repeat([]byte{0x43}, 32), Architecture: "amd64", TTL: time.Hour, RequestID: "request-" + key, Traceparent: testTraceparent}
	approveOperation(t, pool, workspaceID, &request)
	operation, replayed, err := service.CreateSynthetic(ctx, request)
	if err != nil || replayed || operation.State != "queued" {
		t.Fatalf("create approved upgrade = state %q, replayed %v, err %v", operation.State, replayed, err)
	}
	if operation.AgentUpgradeState != "queued" || operation.AgentUpgradeTarget != "2.0.0" {
		t.Fatalf("upgrade projection = %q/%q", operation.AgentUpgradeState, operation.AgentUpgradeTarget)
	}
	var fromVersion string
	if err := pool.QueryRow(ctx, `SELECT from_version FROM agent_upgrade_operations WHERE operation_id=$1`, operation.ID).Scan(&fromVersion); err != nil || fromVersion != "1.2.0" {
		t.Fatalf("from_version = %q, %v", fromVersion, err)
	}
	operationUUID, err := uuid.Parse(operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return operationUUID
}

// acknowledgeUpgradeScheduling simulates the localslice normalization of the
// agent's scheduling acknowledgement: non-terminal accepted with a timestamp.
func acknowledgeUpgradeScheduling(t *testing.T, pool *pgxpool.Pool, operationID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE agent_upgrade_operations SET state='accepted',scheduled_at=now(),updated_at=now() WHERE operation_id=$1`, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='accepted',updated_at=now() WHERE id=$1 AND state IN('queued','dispatched')`, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE commands SET state='accepted',updated_at=now() WHERE operation_id=$1 AND state IN('queued','dispatched')`, operationID); err != nil {
		t.Fatal(err)
	}
}

func reportDurableUpgradeResult(t *testing.T, pool *pgxpool.Pool, operationID, nodeID uuid.UUID, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof) VALUES($1,$2,$3,'2.0.0','',$4,$4,decode('00','hex'))`, operationID, nodeID, state, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func assertUpgradeStates(t *testing.T, pool *pgxpool.Pool, operationID uuid.UUID, operationState, commandState, projectionState string) {
	t.Helper()
	var gotOperation, gotCommand, gotProjection string
	var completed bool
	if err := pool.QueryRow(context.Background(), `SELECT o.state,COALESCE(c.state,''),u.state,u.completed_at IS NOT NULL
		FROM operations o JOIN agent_upgrade_operations u ON u.operation_id=o.id LEFT JOIN commands c ON c.id=o.command_id
		WHERE o.id=$1`, operationID).Scan(&gotOperation, &gotCommand, &gotProjection, &completed); err != nil {
		t.Fatal(err)
	}
	terminal := projectionState == "succeeded" || projectionState == "failed" || projectionState == "rolled_back" || projectionState == "unknown"
	if gotOperation != operationState || gotCommand != commandState || gotProjection != projectionState || completed != terminal {
		t.Fatalf("operation/command/projection/completed = %q/%q/%q/%v, want %q/%q/%q/%v", gotOperation, gotCommand, gotProjection, completed, operationState, commandState, projectionState, terminal)
	}
}

func TestRolloutDispatchRechecksEligibilityAfterClaimIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
	service.EnableReleaseCatalog(rolloutTestCatalog(t))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',true)`, nodeID); err != nil {
		t.Fatal(err)
	}
	rolloutID, approvalID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	requesterID, requesterSessionID, approverID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	requestHash, requestSummary := approvals.AgentRolloutBinding("2.0.0", []uuid.UUID{nodeID}, 1, true)
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'test',$2,now(),now()),($3,'test',$4,now(),now())`, requesterID, requesterID.String(), approverID, approverID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, requesterSessionID, requesterID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,consumed_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.rollout','batch_operation',$4,'integration test','consumed',$5,'independent approval',now()+interval '1 hour',now(),now(),now(),$6,$7)`, approvalID, workspaceID, requesterID, rolloutID, approverID, requestHash, requestSummary); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_rollouts(id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,current_batch,pause_code,exclusions,idempotency_key,created_at,updated_at) VALUES($1,$2,'2.0.0','running',1,true,'integration test',$3,$4,$5,$6,0,'','[]','rollout-post-claim',now(),now())`, rolloutID, workspaceID, approvalID, requestHash, requesterID, requesterSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_rollout_nodes(rollout_id,node_id,ordinal,batch,state,dispatch_node_version,dispatch_attempt,dispatch_lease_until,updated_at) VALUES($1,$2,0,0,'pending',1,1,now()+interval '1 minute',now())`, rolloutID, nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_rollouts WHERE id=$1`, rolloutID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM approval_requests WHERE id=$1`, approvalID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM auth_sessions WHERE id=$1`, requesterSessionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM identities WHERE id IN($1,$2)`, requesterID, approverID)
	})

	// The claim was eligible, but the node changed before operation intent was
	// created. The dispatch fence must turn that late rejection into a pause.
	if _, err := pool.Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	digest := [32]byte{}
	claimed := claimedRolloutNode{rolloutNodeRow: rolloutNodeRow{NodeID: nodeID, Batch: 0, DispatchVersion: 1, DispatchAttempt: 1}, Observation: rolloutNodeObservation{NodeID: nodeID, Status: "active", Architecture: "amd64", AgentVersion: "1.2.0", CapabilityOK: true}, Digest: digest}
	stop, err := service.dispatchRolloutNode(ctx, rolloutID, claimed)
	if err != nil || !stop {
		t.Fatalf("late-ineligible dispatch stop=%v err=%v", stop, err)
	}
	var rolloutState, pauseCode, nodeState, failureCode string
	if err := pool.QueryRow(ctx, `SELECT r.state,r.pause_code,rn.state,rn.failure_code FROM agent_rollouts r JOIN agent_rollout_nodes rn ON rn.rollout_id=r.id WHERE r.id=$1`, rolloutID).Scan(&rolloutState, &pauseCode, &nodeState, &failureCode); err != nil {
		t.Fatal(err)
	}
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_upgrade_operations WHERE approval_id=$1`, approvalID).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if rolloutState != "paused" || pauseCode != "node_skipped" || nodeState != "skipped" || failureCode != "node_unavailable" || operationCount != 0 {
		t.Fatalf("late-ineligible result=%s/%s node=%s/%s operations=%d", rolloutState, pauseCode, nodeState, failureCode, operationCount)
	}
}

func TestAgentUpgradeReconciliationLifecycleIntegration(t *testing.T) {
	t.Run("queued without acknowledgement stays queued", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-queued")
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "queued", "queued", "queued")
	})

	t.Run("a disconnect during the restart window is never a failure", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-offline")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		observeUpgradeNode(t, pool, nodeID, "1.2.0", 15*time.Minute)
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "accepted", "accepted", "accepted")
	})

	t.Run("a reconnect alone is verifying progress, never success", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-reconnect")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		reportDurableUpgradeResult(t, pool, operationID, nodeID, "succeeded")
		// The node reconnected but still reports the old agent version.
		observeUpgradeNode(t, pool, nodeID, "1.2.0", 0)
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "running", "accepted", "running")
	})

	t.Run("success needs the durable outcome and the target version online", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-success")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		reportDurableUpgradeResult(t, pool, operationID, nodeID, "succeeded")
		observeUpgradeNode(t, pool, nodeID, "2.0.0", 0)
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "succeeded", "succeeded", "succeeded")
		var published bool
		if err := pool.QueryRow(context.Background(), `SELECT o.published_at IS NOT NULL FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.operation_id=$1`, operationID).Scan(&published); err != nil || !published {
			t.Fatalf("outbox published = %v, %v", published, err)
		}
		var outcome string
		if err := pool.QueryRow(context.Background(), `SELECT result FROM audit_events WHERE workspace_id=$1 AND action='agent.upgrade' AND actor_type='controller' ORDER BY occurred_at DESC LIMIT 1`, workspaceID).Scan(&outcome); err != nil || outcome != "succeeded" {
			t.Fatalf("controller audit outcome = %q, %v", outcome, err)
		}
		// A terminal upgrade leaves the pending set; reruns are stable.
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "succeeded", "succeeded", "succeeded")
	})

	t.Run("a durable local failure is terminal even while offline", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-failed")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		reportDurableUpgradeResult(t, pool, operationID, nodeID, "failed")
		observeUpgradeNode(t, pool, nodeID, "1.2.0", 15*time.Minute)
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "failed", "failed", "failed")
	})

	t.Run("a durable rollback result is terminal", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-rollback")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		reportDurableUpgradeResult(t, pool, operationID, nodeID, "rolled_back")
		observeUpgradeNode(t, pool, nodeID, "1.2.0", 0)
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "rolled_back", "rolled_back", "rolled_back")
	})

	t.Run("an unresolved upgrade becomes unknown at the deadline", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-unknown")
		acknowledgeUpgradeScheduling(t, pool, operationID)
		// Backdate the acknowledgement beyond the two hour deadline.
		if _, err := pool.Exec(context.Background(), `UPDATE agent_upgrade_operations SET scheduled_at=now()-interval '3 hours' WHERE operation_id=$1`, operationID); err != nil {
			t.Fatal(err)
		}
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "unknown", "expired", "unknown")
		var eventCount, auditCount int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operation_events WHERE operation_id=$1`, operationID).Scan(&eventCount); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE resource_id=$1`, operationID).Scan(&auditCount); err != nil {
			t.Fatal(err)
		}
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "unknown", "expired", "unknown")
		var repeatedEvents, repeatedAudits int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM operation_events WHERE operation_id=$1`, operationID).Scan(&repeatedEvents); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE resource_id=$1`, operationID).Scan(&repeatedAudits); err != nil {
			t.Fatal(err)
		}
		if repeatedEvents != eventCount || repeatedAudits != auditCount {
			t.Fatalf("terminal unknown reconciliation added events/audits: %d/%d -> %d/%d", eventCount, auditCount, repeatedEvents, repeatedAudits)
		}
		createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-after-unknown")
	})

	t.Run("a generic expiry mirrors into an unknown projection", func(t *testing.T) {
		service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
		operationID := createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-generic-expiry")
		if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='expired',updated_at=now() WHERE id=$1`, operationID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE commands SET state='expired',updated_at=now() WHERE operation_id=$1`, operationID); err != nil {
			t.Fatal(err)
		}
		if err := service.ReconcileAgentUpgrades(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertUpgradeStates(t, pool, operationID, "expired", "expired", "unknown")
	})
}

func TestAgentUpgradeActiveConflictIntegration(t *testing.T) {
	service, pool, workspaceID, nodeID := upgradeReconciliationFixture(t)
	createScheduledUpgrade(t, service, pool, workspaceID, nodeID, "upgrade-active-first")
	request := CreateRequest{NodeID: nodeID, IdempotencyKey: "upgrade-active-second", ExpectedVersion: 1, Kind: AgentUpgrade, ActorID: "operator", Action: "agent.upgrade", Reason: "integration test", TargetVersion: "2.0.0", PackageSHA256: bytes.Repeat([]byte{0x43}, 32), Architecture: "amd64", TTL: time.Hour, RequestID: "request-upgrade-active-second", Traceparent: testTraceparent}
	approveOperation(t, pool, workspaceID, &request)
	if _, _, err := service.CreateSynthetic(context.Background(), request); !errors.Is(err, ErrUpgradeActive) {
		t.Fatalf("second concurrent upgrade error = %v", err)
	}
	// Once the first upgrade reaches a terminal outcome the node is eligible
	// again, including after the conservative unknown outcome.
	if _, err := pool.Exec(context.Background(), `UPDATE operations SET state='failed',updated_at=now() WHERE node_id=$1 AND state='queued'`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE agent_upgrade_operations SET state='failed',completed_at=now(),updated_at=now() WHERE node_id=$1 AND state='queued'`, nodeID); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey, request.RequestID = "upgrade-active-third", "request-upgrade-active-third"
	approveOperation(t, pool, workspaceID, &request)
	operation, replayed, err := service.CreateSynthetic(context.Background(), request)
	if err != nil || replayed || operation.State != "queued" {
		t.Fatalf("upgrade after terminal predecessor = state %q, replayed %v, err %v", operation.State, replayed, err)
	}
}
