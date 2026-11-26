package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) LockRolloutClaim(ctx context.Context, rollout, workspace, node uuid.UUID) (v operationstore.RolloutClaim, err error) {
	err = s.QueryRow(ctx, `SELECT r.state,rn.state,rn.dispatch_lease_until FROM agent_rollouts r JOIN agent_rollout_nodes rn ON rn.rollout_id=r.id WHERE r.id=$1 AND r.workspace_id=$2 AND rn.node_id=$3 FOR UPDATE OF r,rn`, rollout, workspace, node).Scan(&v.State, &v.NodeState, &v.DispatchLease)
	return
}

func (s operationStore) UpgradeNode(ctx context.Context, node uuid.UUID) (workspace uuid.UUID, architecture, osRelease string, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id,COALESCE(o.architecture,''),COALESCE(o.os_release,'') FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1`, node).Scan(&workspace, &architecture, &osRelease)
	return
}

func (s operationStore) LockAgentObservation(ctx context.Context, node uuid.UUID) (v operationstore.AgentObservation, err error) {
	err = s.QueryRow(ctx, `SELECT architecture,agent_version,os_release,last_heartbeat_at FROM node_observed_snapshots WHERE node_id=$1 FOR UPDATE`, node).Scan(&v.Architecture, &v.AgentVersion, &v.OSRelease, &v.LastHeartbeatAt)
	return
}

func (s operationStore) LockUpgradeCapability(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT approved FROM node_capabilities WHERE node_id=$1 AND capability IN ('ocserv.agent.upgrade.v1','ocserv.agent.upgrade.v2') ORDER BY approved DESC,capability DESC LIMIT 1 FOR UPDATE`, node).Scan(&v)
	return
}

func (s operationStore) HasActiveUpgrade(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_upgrade_operations WHERE node_id=$1 AND completed_at IS NULL AND state IN ('queued','accepted','running','unknown'))`, node).Scan(&v)
	return
}

func (s operationStore) RolloutObservation(ctx context.Context, node, workspace uuid.UUID) (v operationstore.RolloutObservation, err error) {
	v.NodeID = node
	err = s.QueryRow(ctx, `SELECT n.status,COALESCE(o.architecture,''),COALESCE(o.agent_version,''),COALESCE(o.os_release,''),o.last_heartbeat_at,
		EXISTS(SELECT 1 FROM node_capabilities c WHERE c.node_id=n.id AND c.capability IN ('ocserv.agent.upgrade.v1','ocserv.agent.upgrade.v2') AND c.approved=true),
		EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.completed_at IS NULL AND u.state IN ('queued','accepted','running','unknown'))
		FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1 AND n.workspace_id=$2`, node, workspace).Scan(&v.Status, &v.Architecture, &v.AgentVersion, &v.OSRelease, &v.LastHeartbeatAt, &v.CapabilityOK, &v.UpgradeActive)
	return
}

func (s operationStore) InsertUpgrade(ctx context.Context, v operationstore.PendingUpgrade) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,from_version,state,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,'queued',$8,$8)`, v.ID, v.WorkspaceID, v.NodeID, v.TargetVersion, v.PackageSHA256, v.Architecture, v.FromVersion, v.CreatedAt)
	return err
}

func (s operationStore) PendingUpgrades(ctx context.Context, limit int) ([]operationstore.ScheduledUpgrade, error) {
	return collectRows(ctx, s.store, `SELECT u.operation_id,u.workspace_id,u.node_id,op.command_id,u.target_version,u.from_version,u.state,COALESCE(u.scheduled_at,u.created_at),u.created_at,
		op.state,n.status,COALESCE(snap.agent_version,''),snap.observed_at,snap.last_heartbeat_at,COALESCE(r.state,''),COALESCE(r.detail,'')
		FROM agent_upgrade_operations u JOIN operations op ON op.id=u.operation_id JOIN nodes n ON n.id=u.node_id
		LEFT JOIN node_observed_snapshots snap ON snap.node_id=u.node_id LEFT JOIN node_agent_upgrade_results r ON r.operation_id=u.operation_id
		WHERE u.completed_at IS NULL AND u.state IN ('queued','accepted','running','unknown') LIMIT $1`, func(r Rows) (v operationstore.ScheduledUpgrade, err error) {
		err = r.Scan(&v.OperationID, &v.WorkspaceID, &v.NodeID, &v.CommandID, &v.TargetVersion, &v.FromVersion, &v.State, &v.ScheduledAt, &v.CreatedAt, &v.OperationState, &v.NodeStatus, &v.ObservedVersion, &v.ObservedAt, &v.LastHeartbeatAt, &v.DurableState, &v.DurableDetail)
		return
	}, limit)
}

// A lost conditional update aborts the caller's transaction, including any
// preceding projection writes, rather than committing a partial transition.
func (s operationStore) UpgradeTerminal(ctx context.Context, v operationstore.UpgradeTransition) error {
	if !v.GenericTerminal && v.State != v.OperationState {
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, v.CommandID, v.At); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE commands SET state=$2,updated_at=$3 WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, v.CommandID, v.CommandState, v.At); err != nil {
			return err
		}
		count, err := s.Exec(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=$3,completed_at=GREATEST(COALESCE(completed_at,$3),$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, v.OperationID, v.State, v.At)
		if err != nil {
			return err
		}
		if count == 0 {
			return database.ErrNotFound
		}
	}
	count, err := s.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,completed_at=$3,updated_at=$3 WHERE operation_id=$1 AND completed_at IS NULL AND state IN ('queued','accepted','running','unknown')`, v.OperationID, v.State, v.At)
	if err != nil {
		return err
	}
	if count == 0 {
		return database.ErrNotFound
	}
	return s.upgradeEvent(ctx, v)
}

func (s operationStore) UpgradeProgress(ctx context.Context, v operationstore.UpgradeTransition) error {
	count, err := s.Exec(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=$3 WHERE id=$1 AND state='accepted'`, v.OperationID, v.State, v.At)
	if err != nil {
		return err
	}
	if count == 0 {
		return database.ErrNotFound
	}
	if _, err := s.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,updated_at=$3 WHERE operation_id=$1 AND state='accepted'`, v.OperationID, v.State, v.At); err != nil {
		return err
	}
	return s.upgradeEvent(ctx, v)
}

func (s operationStore) upgradeEvent(ctx context.Context, v operationstore.UpgradeTransition) error {
	event, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,$3,$4)`, event, v.OperationID, v.State, v.At)
	return err
}
