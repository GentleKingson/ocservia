package postgres

import (
	"context"
	"encoding/json"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

const rolloutColumns = `id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,current_batch,pause_code,exclusions,created_at,updated_at`

func scanRollout(row database.Row) (v operationstore.Rollout, err error) {
	var exclusions value.JSONB
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.TargetVersion, &v.State, &v.BatchSize, &v.StopOnFailure, &v.Reason, &v.ApprovalID, &v.RequestHash, &v.CreatedBy, &v.ActorSession, &v.CurrentBatch, &v.PauseCode, &exclusions, &v.CreatedAt, &v.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(exclusions.Bytes(), &v.Excluded)
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

func (s operationStore) FindRollout(ctx context.Context, workspace uuid.UUID, key string) (id uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT id,request_hash FROM agent_rollouts WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, key).Scan(&id, &hash)
	return
}

func (s operationStore) RolloutApproval(ctx context.Context, id, workspace, requester uuid.UUID, hash []byte, at value.Timestamp) (resource uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT resource_id FROM approval_requests WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action='agent.rollout' AND resource_type='batch_operation' AND status='approved' AND expires_at>$5 AND request_hash=$4 FOR UPDATE`, id, workspace, requester, hash, at).Scan(&resource)
	return
}

func (s operationStore) InsertRollout(ctx context.Context, v operationstore.PendingRollout) error {
	exclusions, err := json.Marshal(v.Excluded)
	if err != nil {
		return err
	}
	document, err := value.ParseJSONB(exclusions)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO agent_rollouts(id,workspace_id,target_version,state,batch_size,stop_on_failure,reason,approval_id,request_hash,created_by,actor_session_id,current_batch,pause_code,exclusions,idempotency_key,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,true,$5,$6,$7,$8,$9,0,'',$10,$11,$12,$12)`, v.ID, v.WorkspaceID, v.TargetVersion, v.BatchSize, v.Reason, v.ApprovalID, v.RequestHash, v.CreatedBy, v.ActorSession, document, v.IdempotencyKey, v.CreatedAt)
	return err
}

func (s operationStore) InsertRolloutNode(ctx context.Context, id uuid.UUID, node operationstore.RolloutNode, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_rollout_nodes(rollout_id,node_id,ordinal,batch,state,from_version,updated_at) VALUES($1,$2,$3,$4,'pending','',$5)`, id, node.NodeID, node.Ordinal, node.Batch, at)
	return err
}
