package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) PendingUpgrades(ctx context.Context, limit int) ([]operationstore.ScheduledUpgrade, error) {
	rows, err := s.Query(ctx, `SELECT u.operation_id,u.workspace_id,u.node_id,op.command_id,u.target_version,u.from_version,u.state,COALESCE(u.scheduled_at,u.created_at),u.created_at,
		op.state,n.status,COALESCE(snap.agent_version,''),snap.observed_at,snap.last_heartbeat_at,COALESCE(r.state,''),COALESCE(r.detail,'')
		FROM agent_upgrade_operations u JOIN operations op ON op.id=u.operation_id JOIN nodes n ON n.id=u.node_id
		LEFT JOIN node_observed_snapshots snap ON snap.node_id=u.node_id LEFT JOIN node_agent_upgrade_results r ON r.operation_id=u.operation_id
		WHERE u.completed_at IS NULL AND u.state IN ('queued','accepted','running','unknown') LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []operationstore.ScheduledUpgrade
	for rows.Next() {
		var v operationstore.ScheduledUpgrade
		if err := rows.Scan(&v.OperationID, &v.WorkspaceID, &v.NodeID, &v.CommandID, &v.TargetVersion, &v.FromVersion, &v.State, &v.ScheduledAt, &v.CreatedAt, &v.OperationState, &v.NodeStatus, &v.ObservedVersion, &v.ObservedAt, &v.LastHeartbeatAt, &v.DurableState, &v.DurableDetail); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

// A lost conditional update aborts the caller's transaction, including any
// preceding projection writes, rather than committing a partial transition.
func (s operationStore) UpgradeTerminal(ctx context.Context, v operationstore.UpgradeTransition) error {
	if !v.GenericTerminal && v.State != v.OperationState {
		count, err := s.Exec(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=$3,completed_at=GREATEST(COALESCE(completed_at,$3),$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, v.OperationID, v.State, v.At)
		if err != nil {
			return err
		}
		if count == 0 {
			return database.ErrNotFound
		}
		if _, err := s.Exec(ctx, `UPDATE commands SET state=$2,updated_at=$3 WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, v.CommandID, v.CommandState, v.At); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, v.CommandID, v.At); err != nil {
			return err
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
