package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

type userOperationsStore struct{ database.Tx }

func (t *transaction) UserOperationsStore() userstore.Store { return userOperationsStore{t} }

func (s userOperationsStore) LockUser(ctx context.Context, node uuid.UUID, name string) (workspace uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT n.workspace_id FROM desired_users u JOIN nodes n ON n.id=u.node_id WHERE u.node_id=$1 AND u.username=$2 AND n.status IN('active','offline') FOR UPDATE OF u`, node, name).Scan(&workspace)
	return
}

func (s userOperationsStore) Mutation(ctx context.Context, workspace uuid.UUID, key string) (v userstore.Mutation, err error) {
	err = s.QueryRow(ctx, `SELECT policy_version,request_hash FROM user_policy_mutations WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, key).Scan(&v.Version, &v.Hash)
	return
}

func (s userOperationsStore) LockPolicy(ctx context.Context, node uuid.UUID, name string) (version int64, err error) {
	err = s.QueryRow(ctx, `SELECT version FROM desired_user_policies WHERE node_id=$1 AND username=$2 FOR UPDATE`, node, name).Scan(&version)
	return
}

func (s userOperationsStore) PutPolicy(ctx context.Context, v userstore.Policy) error {
	_, err := s.Exec(ctx, `INSERT INTO desired_user_policies(node_id,username,quota_period,quota_direction,quota_bytes,expires_at,version,created_at,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(node_id,username) DO UPDATE SET quota_period=EXCLUDED.quota_period,quota_direction=EXCLUDED.quota_direction,quota_bytes=EXCLUDED.quota_bytes,expires_at=EXCLUDED.expires_at,version=EXCLUDED.version,updated_at=EXCLUDED.updated_at`, v.NodeID, v.Username, v.QuotaPeriod, v.QuotaDirection, v.QuotaBytes, v.ExpiresAt, v.Version, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s userOperationsStore) InsertMutation(ctx context.Context, v userstore.Mutation) error {
	_, err := s.Exec(ctx, `INSERT INTO user_policy_mutations(id,workspace_id,node_id,username,idempotency_key,request_hash,policy_version,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, v.ID, v.WorkspaceID, v.NodeID, v.Username, v.IdempotencyKey, v.Hash, v.Version, v.At)
	return err
}

func (s userOperationsStore) Policy(ctx context.Context, node uuid.UUID, name string, month value.Timestamp) (v userstore.PolicyState, err error) {
	v.NodeID, v.Username = node, name
	err = s.QueryRow(ctx, `SELECT p.quota_period,p.quota_direction,p.quota_bytes,p.expires_at,p.version,p.created_at,p.updated_at,
 CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END,
 COALESCE(u.rx_bytes,0),COALESCE(u.tx_bytes,0),u.observed_at,n.status,d.enabled,d.revision,o.enabled,o.revision,latest.state
 FROM desired_user_policies p JOIN nodes n ON n.id=p.node_id JOIN desired_users d ON d.node_id=p.node_id AND d.username=p.username
 LEFT JOIN observed_users o ON o.node_id=p.node_id AND o.username=p.username
 LEFT JOIN observed_user_usage u ON u.node_id=p.node_id AND u.username=p.username AND u.period=CASE WHEN p.quota_period='monthly' THEN 'monthly' ELSE 'lifetime' END AND u.period_start=CASE WHEN p.quota_period='monthly' THEN $3::timestamptz ELSE '1970-01-01T00:00:00Z'::timestamptz END
 LEFT JOIN LATERAL (SELECT op.state FROM commands command JOIN operations op ON op.id=command.operation_id WHERE command.node_id=p.node_id AND command.resource_type='user' AND command.resource_key=p.username ORDER BY command.created_at DESC,command.id DESC LIMIT 1) latest ON true
 WHERE p.node_id=$1 AND p.username=$2`, node, name, month).Scan(&v.QuotaPeriod, &v.QuotaDirection, &v.QuotaBytes, &v.ExpiresAt, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.PeriodStart, &v.ObservedRXBytes, &v.ObservedTXBytes, &v.ObservedAt, &v.NodeStatus, &v.DesiredEnabled, &v.DesiredRevision, &v.ObservedEnabled, &v.ObservedRevision, &v.OperationState)
	return
}

func (s userOperationsStore) BatchApproval(ctx context.Context, id, workspace, actor uuid.UUID) (resource uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT resource_id,request_hash FROM approval_requests WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action='user.batch.disable' AND resource_type='batch_operation'`, id, workspace, actor).Scan(&resource, &hash)
	return
}

func (s userOperationsStore) ApprovalItems(ctx context.Context, id uuid.UUID) ([]userstore.ApprovalItem, error) {
	rows, err := s.Query(ctx, `SELECT node_id,username,action,expected_version FROM approval_batch_items WHERE approval_id=$1 ORDER BY item_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []userstore.ApprovalItem
	for rows.Next() {
		var v userstore.ApprovalItem
		if err := rows.Scan(&v.NodeID, &v.Username, &v.Action, &v.ExpectedVersion); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s userOperationsStore) BatchByKey(ctx context.Context, workspace uuid.UUID, key string) (id uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT id,request_hash FROM batch_operations WHERE workspace_id=$1 AND idempotency_key=$2`, workspace, key).Scan(&id, &hash)
	return
}

func (s userOperationsStore) InsertBatch(ctx context.Context, v userstore.Batch) error {
	_, err := s.Exec(ctx, `INSERT INTO batch_operations(id,workspace_id,state,actor_identity_id,actor_session_id,approval_id,actor_id,reason,request_id,traceparent,idempotency_key,request_hash,created_at,updated_at)VALUES($1,$2,'queued',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, v.ID, v.WorkspaceID, v.ActorIdentityID, v.ActorSessionID, v.ApprovalID, v.ActorID, v.Reason, v.RequestID, v.Traceparent, v.IdempotencyKey, v.Hash, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s userOperationsStore) UserExists(ctx context.Context, node uuid.UUID, name string, workspace uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM desired_users u JOIN nodes n ON n.id=u.node_id WHERE u.node_id=$1 AND u.username=$2 AND n.workspace_id=$3)`, node, name, workspace).Scan(&v)
	return
}

func (s userOperationsStore) InsertBatchItem(ctx context.Context, v userstore.BatchItem) error {
	_, err := s.Exec(ctx, `INSERT INTO batch_operation_items(batch_id,item_index,node_id,username,action,expected_version,state,error_type,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`, v.BatchID, v.Index, v.NodeID, v.Username, v.Action, v.ExpectedVersion, v.State, v.ErrorType, v.At)
	return err
}

func (s userOperationsStore) Batch(ctx context.Context, id uuid.UUID) (v userstore.Batch, err error) {
	v.ID = id
	err = s.QueryRow(ctx, `SELECT workspace_id,actor_identity_id,actor_session_id,state,actor_id,reason,request_id,traceparent,created_at,updated_at FROM batch_operations WHERE id=$1`, id).Scan(&v.WorkspaceID, &v.ActorIdentityID, &v.ActorSessionID, &v.State, &v.ActorID, &v.Reason, &v.RequestID, &v.Traceparent, &v.CreatedAt, &v.UpdatedAt)
	return
}

func (s userOperationsStore) BatchItems(ctx context.Context, id uuid.UUID) ([]userstore.BatchItem, error) {
	rows, err := s.Query(ctx, `SELECT item_index,node_id,username,action,expected_version,state,child_operation_id,COALESCE(error_type,''),updated_at FROM batch_operation_items WHERE batch_id=$1 ORDER BY item_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []userstore.BatchItem
	for rows.Next() {
		v := userstore.BatchItem{BatchID: id}
		if err := rows.Scan(&v.Index, &v.NodeID, &v.Username, &v.Action, &v.ExpectedVersion, &v.State, &v.ChildOperationID, &v.ErrorType, &v.At); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
