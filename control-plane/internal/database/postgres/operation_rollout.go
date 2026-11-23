package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

const rolloutColumns = `id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,request_hash,created_by,actor_session_id,current_batch,pause_code,exclusions,created_at,updated_at`

func scanRollout(row database.Row) (v operationstore.Rollout, err error) {
	var exclusions json.RawMessage
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.TargetVersion, &v.State, &v.BatchSize, &v.StopOnFailure, &v.Reason, &v.RequestHash, &v.CreatedBy, &v.ActorSession, &v.CurrentBatch, &v.PauseCode, &exclusions, &v.CreatedAt, &v.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(exclusions, &v.Excluded)
	}
	return
}

func (s operationStore) Rollout(ctx context.Context, id uuid.UUID, lock bool) (operationstore.Rollout, error) {
	query := `SELECT ` + rolloutColumns + ` FROM agent_rollouts WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanRollout(s.QueryRow(ctx, query, id))
}

func (s operationStore) Rollouts(ctx context.Context, workspace uuid.UUID, limit int) ([]operationstore.Rollout, error) {
	rows, err := s.Query(ctx, `SELECT `+rolloutColumns+` FROM agent_rollouts WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT $2`, workspace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []operationstore.Rollout{}
	for rows.Next() {
		v, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) RolloutNodes(ctx context.Context, id uuid.UUID) ([]operationstore.RolloutNode, error) {
	rows, err := s.Query(ctx, `SELECT node_id,ordinal,batch,state,operation_id,from_version,failure_code,COALESCE(dispatch_node_version,0),dispatch_attempt,dispatch_lease_until FROM agent_rollout_nodes WHERE rollout_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []operationstore.RolloutNode{}
	for rows.Next() {
		var v operationstore.RolloutNode
		var operation *uuid.UUID
		if err := rows.Scan(&v.NodeID, &v.Ordinal, &v.Batch, &v.State, &operation, &v.FromVersion, &v.FailureCode, &v.DispatchVersion, &v.DispatchAttempt, &v.DispatchLease); err != nil {
			return nil, err
		}
		if operation != nil {
			v.OperationID = *operation
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) RolloutWorkspace(ctx context.Context, id uuid.UUID) (workspace uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM agent_rollouts WHERE id=$1`, id).Scan(&workspace)
	return
}

func (s operationStore) LockRolloutIntent(ctx context.Context, workspace uuid.UUID, key string) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ocservia/rollout/' || $1::uuid::text || '/' || $2::text, 0))`, workspace, key)
	return err
}

func (s operationStore) FindRollout(ctx context.Context, workspace uuid.UUID, key string) (id uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT id,request_hash FROM agent_rollouts WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, key).Scan(&id, &hash)
	return
}

func (s operationStore) InsertRollout(ctx context.Context, v operationstore.PendingRollout) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_rollouts(id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,request_hash,created_by,actor_session_id,current_batch,pause_code,exclusions,idempotency_key,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,true,$5,$6,$7,$8,0,'',$9,$10,$11,$11)`, v.ID, v.WorkspaceID, v.TargetVersion, v.BatchSize, v.Reason, v.RequestHash, v.CreatedBy, v.ActorSession, v.Exclusions, v.IdempotencyKey, v.CreatedAt)
	return err
}

func (s operationStore) InsertRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, at time.Time) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_rollout_nodes(rollout_id,node_id,ordinal,batch,state,from_version,updated_at) VALUES($1,$2,$3,$4,'pending','',$5)`, id, node.NodeID, node.Ordinal, node.Batch, at)
	return err
}

func (s operationStore) ResumeRollout(ctx context.Context, id uuid.UUID, batch int, at time.Time) (int64, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='pending',failure_code='',dispatch_node_version=NULL,dispatch_attempt=dispatch_attempt+1,dispatch_lease_until=NULL,updated_at=$3 WHERE rollout_id=$1 AND batch=$2 AND (state IN ('failed','rolled_back','unknown') OR (state='skipped' AND batch=0))`, id, batch, at)
	if err != nil {
		return 0, err
	}
	_, err = s.Exec(ctx, `UPDATE agent_rollouts SET state='running',pause_code='',updated_at=$2 WHERE id=$1 AND state='paused'`, id, at)
	return n, err
}

func (s operationStore) ActiveRollouts(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT id FROM agent_rollouts WHERE state IN ('queued','running') ORDER BY created_at LIMIT $1`, limit))
}

func (s operationStore) SetRolloutState(ctx context.Context, id uuid.UUID, state, code string, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollouts SET state=$2,pause_code=$3,updated_at=$4 WHERE id=$1`, id, state, code, at)
	return err
}

func (s operationStore) SetRolloutBatch(ctx context.Context, id uuid.UUID, batch int, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollouts SET current_batch=$2,updated_at=$3 WHERE id=$1`, id, batch, at)
	return err
}

func (s operationStore) TerminalRolloutNodes(ctx context.Context, id uuid.UUID) ([]operationstore.RolloutTerminalNode, error) {
	rows, err := s.Query(ctx, `SELECT rn.node_id,rn.ordinal,rn.batch,rn.operation_id,u.state FROM agent_rollout_nodes rn JOIN operations op ON op.id=rn.operation_id JOIN agent_upgrade_operations u ON u.operation_id=rn.operation_id WHERE rn.rollout_id=$1 AND rn.state='running' AND u.completed_at IS NOT NULL`, id)
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

func (s operationStore) SetRolloutNodeOutcome(ctx context.Context, id, node uuid.UUID, state, code string, at time.Time) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state=$3,failure_code=$4,updated_at=$5 WHERE rollout_id=$1 AND node_id=$2 AND state='running'`, id, node, state, code, at)
	return n > 0, err
}

func (s operationStore) RolloutNodeVersion(ctx context.Context, node uuid.UUID) (version int64, err error) {
	err = s.QueryRow(ctx, `SELECT version FROM nodes WHERE id=$1`, node).Scan(&version)
	return
}

func (s operationStore) ClaimRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, lease, at time.Time) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_node_version=$3,dispatch_attempt=$4,dispatch_lease_until=$5,updated_at=$6 WHERE rollout_id=$1 AND node_id=$2 AND state='pending' AND (dispatch_lease_until IS NULL OR dispatch_lease_until<=$6)`, id, node.NodeID, node.DispatchVersion, node.DispatchAttempt, lease, at)
	return n > 0, err
}

func (s operationStore) SkipRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, reason string, at time.Time) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='skipped',failure_code=$3,dispatch_lease_until=NULL,updated_at=$4 WHERE rollout_id=$1 AND node_id=$2 AND state='pending'`, id, node.NodeID, reason, at)
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_lease_until=NULL,updated_at=$3 WHERE rollout_id=$1 AND batch=$2 AND state='pending'`, id, node.Batch, at); err != nil {
		return false, err
	}
	return true, s.SetRolloutState(ctx, id, "paused", "node_skipped", at)
}

func (s operationStore) AttachRolloutOperation(ctx context.Context, id, node, operation uuid.UUID, from string, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET state='running',operation_id=$3,from_version=$4,dispatch_lease_until=NULL,updated_at=$5 WHERE rollout_id=$1 AND node_id=$2 AND state='pending'`, id, node, operation, from, at)
	return err
}

func (s operationStore) LockRolloutNode(ctx context.Context, id, node uuid.UUID) (v operationstore.RolloutNode, err error) {
	v.NodeID = node
	err = s.QueryRow(ctx, `SELECT batch,state FROM agent_rollout_nodes WHERE rollout_id=$1 AND node_id=$2 FOR UPDATE`, id, node).Scan(&v.Batch, &v.State)
	return
}

func (s operationStore) ReleaseRolloutClaim(ctx context.Context, id, node uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE agent_rollout_nodes SET dispatch_lease_until=NULL,updated_at=$3 WHERE rollout_id=$1 AND node_id=$2`, id, node, at)
	return err
}
