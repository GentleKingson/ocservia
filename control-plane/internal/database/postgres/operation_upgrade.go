package postgres

import (
	"context"

	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) LockRolloutClaim(ctx context.Context, rollout, workspace, node uuid.UUID) (v operationstore.RolloutClaim, err error) {
	err = s.QueryRow(ctx, `SELECT r.state,rn.state,rn.dispatch_lease_until FROM agent_rollouts r JOIN agent_rollout_nodes rn ON rn.rollout_id=r.id WHERE r.id=$1 AND r.workspace_id=$2 AND rn.node_id=$3 FOR UPDATE OF r,rn`, rollout, workspace, node).Scan(&v.State, &v.NodeState, &v.DispatchLease)
	return
}

func (s operationStore) LockAgentObservation(ctx context.Context, node uuid.UUID) (v operationstore.AgentObservation, err error) {
	err = s.QueryRow(ctx, `SELECT architecture,agent_version,last_heartbeat_at FROM node_observed_snapshots WHERE node_id=$1 FOR UPDATE`, node).Scan(&v.Architecture, &v.AgentVersion, &v.LastHeartbeatAt)
	return
}

func (s operationStore) LockUpgradeCapability(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT approved FROM node_capabilities WHERE node_id=$1 AND capability='ocserv.agent.upgrade.v2' FOR UPDATE`, node).Scan(&v)
	return
}

func (s operationStore) HasActiveUpgrade(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_upgrade_operations WHERE node_id=$1 AND completed_at IS NULL AND state IN ('queued','accepted','running','unknown'))`, node).Scan(&v)
	return
}

func (s operationStore) RolloutObservation(ctx context.Context, node, workspace uuid.UUID) (v operationstore.RolloutObservation, err error) {
	v.NodeID = node
	err = s.QueryRow(ctx, `SELECT n.status,COALESCE(o.architecture,''),COALESCE(o.agent_version,''),o.last_heartbeat_at,
		EXISTS(SELECT 1 FROM node_capabilities c WHERE c.node_id=n.id AND c.capability='ocserv.agent.upgrade.v2' AND c.approved=true),
		EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.completed_at IS NULL AND u.state IN ('queued','accepted','running','unknown'))
		FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1 AND n.workspace_id=$2`, node, workspace).Scan(&v.Status, &v.Architecture, &v.AgentVersion, &v.LastHeartbeatAt, &v.CapabilityOK, &v.UpgradeActive)
	return
}

func (s operationStore) InsertUpgrade(ctx context.Context, v operationstore.PendingUpgrade) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_upgrade_operations(operation_id,workspace_id,node_id,target_version,package_sha256,architecture,from_version,approval_id,state,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,'queued',$9,$9)`, v.ID, v.WorkspaceID, v.NodeID, v.TargetVersion, v.PackageSHA256, v.Architecture, v.FromVersion, v.ApprovalID, v.CreatedAt)
	return err
}
