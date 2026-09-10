package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) ResumeRollout(ctx context.Context, id uuid.UUID, batch int, at value.Timestamp) (int64, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='pending',failure_code='',dispatch_node_version=NULL,dispatch_attempt=dispatch_attempt+1,dispatch_lease_until=NULL,updated_at=? WHERE rollout_id=? AND batch=? AND (state IN ('failed','rolled_back','unknown') OR (state='skipped' AND batch=0))`, at, UUIDBytes(id), batch)
	if err != nil {
		return 0, err
	}
	_, err = s.Exec(ctx, `UPDATE agent_rollouts SET state='running',pause_code='',updated_at=? WHERE id=? AND state='paused'`, at, UUIDBytes(id))
	return n, err
}

func (s operationStore) ActiveRollouts(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.Query(ctx, `SELECT id FROM agent_rollouts WHERE state IN ('queued','running') ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		values = append(values, id)
	}
	return values, rows.Err()
}

func (s operationStore) SetRolloutState(ctx context.Context, id uuid.UUID, state, code string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollouts SET state=?,pause_code=?,updated_at=? WHERE id=?`, state, code, at, UUIDBytes(id))
	return err
}

func (s operationStore) SetRolloutBatch(ctx context.Context, id uuid.UUID, batch int, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollouts SET current_batch=?,updated_at=? WHERE id=?`, batch, at, UUIDBytes(id))
	return err
}

func (s operationStore) TerminalRolloutNodes(ctx context.Context, id uuid.UUID) ([]operationstore.RolloutTerminalNode, error) {
	rows, err := s.Query(ctx, `SELECT rn.node_id,rn.ordinal,rn.batch,rn.operation_id,u.state FROM agent_rollout_nodes rn JOIN operations op ON op.id=rn.operation_id JOIN agent_upgrade_operations u ON u.operation_id=rn.operation_id WHERE rn.rollout_id=? AND rn.state='running' AND u.completed_at IS NOT NULL`, UUIDBytes(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []operationstore.RolloutTerminalNode{}
	for rows.Next() {
		var v operationstore.RolloutTerminalNode
		if err := rows.Scan(&v.NodeID, &v.Ordinal, &v.Batch, &v.OperationID, &v.Outcome); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) SetRolloutNodeOutcome(ctx context.Context, id, node uuid.UUID, state, code string, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state=?,failure_code=?,updated_at=? WHERE rollout_id=? AND node_id=? AND state='running'`, state, code, at, UUIDBytes(id), UUIDBytes(node))
	return n > 0, err
}

func (s operationStore) RolloutNodeVersion(ctx context.Context, node uuid.UUID) (version int64, err error) {
	err = s.QueryRow(ctx, `SELECT version FROM nodes WHERE id=?`, UUIDBytes(node)).Scan(&version)
	return
}

func (s operationStore) ClaimRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, lease, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_node_version=?,dispatch_attempt=?,dispatch_lease_until=?,updated_at=? WHERE rollout_id=? AND node_id=? AND state='pending' AND (dispatch_lease_until IS NULL OR dispatch_lease_until<=?)`, node.DispatchVersion, node.DispatchAttempt, lease, at, UUIDBytes(id), UUIDBytes(node.NodeID), at)
	return n > 0, err
}

func (s operationStore) SkipRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, reason string, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='skipped',failure_code=?,dispatch_lease_until=NULL,updated_at=? WHERE rollout_id=? AND node_id=? AND state='pending'`, reason, at, UUIDBytes(id), UUIDBytes(node.NodeID))
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_lease_until=NULL,updated_at=? WHERE rollout_id=? AND batch=? AND state='pending'`, at, UUIDBytes(id), node.Batch); err != nil {
		return false, err
	}
	return true, s.SetRolloutState(ctx, id, "paused", "node_skipped", at)
}

func (s operationStore) AttachRolloutOperation(ctx context.Context, id, node, operation uuid.UUID, from string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='running',operation_id=?,from_version=?,dispatch_lease_until=NULL,updated_at=? WHERE rollout_id=? AND node_id=? AND state='pending'`, UUIDBytes(operation), from, at, UUIDBytes(id), UUIDBytes(node))
	return err
}

func (s operationStore) LockRolloutNode(ctx context.Context, id, node uuid.UUID) (v operationstore.RolloutNode, err error) {
	v.NodeID = node
	err = s.QueryRow(ctx, `SELECT batch,state FROM agent_rollout_nodes WHERE rollout_id=? AND node_id=? FOR UPDATE`, UUIDBytes(id), UUIDBytes(node)).Scan(&v.Batch, &v.State)
	return
}

func (s operationStore) ReleaseRolloutClaim(ctx context.Context, id, node uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_lease_until=NULL,updated_at=? WHERE rollout_id=? AND node_id=?`, at, UUIDBytes(id), UUIDBytes(node))
	return err
}
