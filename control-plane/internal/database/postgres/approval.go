package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"time"
)

type approvalStore struct{ database.Store }

func (b *Backend) ApprovalStore() approvalstore.Store     { return approvalStore{b} }
func (t *transaction) ApprovalStore() approvalstore.Store { return approvalStore{t} }
func (s approvalStore) ConsumeBound(ctx context.Context, id, workspace, requester uuid.UUID, action, resourceType string, resource uuid.UUID, hash []byte) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE approval_requests SET status='consumed',consumed_at=now() WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6 AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND approver_id IS DISTINCT FROM requester_id AND expires_at>now() AND request_hash=$7`, id, workspace, requester, action, resourceType, resource, hash)
	return n == 1, err
}

func (s approvalStore) Insert(ctx context.Context, args []any) error {
	_, err := s.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary,authority_snapshot_at) VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8,$9,$10,$11,$9)`, args...)
	return err
}
func (s approvalStore) AddAuthority(ctx context.Context, id, workspace uuid.UUID, kind string, resource uuid.UUID) error {
	_, err := s.Exec(ctx, `INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, workspace, kind, resource)
	return err
}
func (s approvalStore) AddBatchItem(ctx context.Context, id uuid.UUID, index int, node uuid.UUID, username, action string, version int64) error {
	_, err := s.Exec(ctx, `INSERT INTO approval_batch_items(approval_id,item_index,node_id,username,action,expected_version) VALUES($1,$2,$3,$4,$5,$6)`, id, index, node, username, action, version)
	return err
}
func (s approvalStore) Get(ctx context.Context, id uuid.UUID, lock bool) database.Row {
	q := `SELECT id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,COALESCE(encode(request_hash,'hex'),''),request_summary FROM approval_requests WHERE id=$1`
	if lock {
		q += ` FOR UPDATE`
	}
	return s.QueryRow(ctx, q, id)
}
func (s approvalStore) Authorized(ctx context.Context, id, actor uuid.UUID) (bool, error) {
	var valid bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_authority_resources WHERE approval_id=$1) AND NOT EXISTS (SELECT 1 FROM approval_authority_resources scope WHERE scope.approval_id=$1 AND NOT EXISTS (SELECT 1 FROM role_bindings binding WHERE binding.identity_id=$2 AND binding.workspace_id=scope.workspace_id AND binding.created_at <= (SELECT authority_snapshot_at FROM approval_requests WHERE id=$1) AND binding.role_name IN ('SecurityAdmin','PlatformAdmin') AND (binding.resource_type='workspace' OR (binding.resource_type=scope.resource_type AND binding.resource_id=scope.resource_id))))`, id, actor).Scan(&valid)
	return valid, err
}
func (s approvalStore) Approve(ctx context.Context, id, actor uuid.UUID, reason string, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE approval_requests SET status='approved',approver_id=$2,approval_reason=$3,approved_at=$4 WHERE id=$1`, id, actor, reason, at)
	return err
}
func (s approvalStore) ValidBound(ctx context.Context, id, workspace, requester uuid.UUID, action, kind string, resource uuid.UUID, hash []byte, consumed bool) (bool, error) {
	status, expiry := "approved", ` AND expires_at>now()`
	if consumed {
		status, expiry = "consumed", ""
	}
	var valid bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_requests WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6 AND request_hash=$7 AND status=$8 AND approver_id IS DISTINCT FROM requester_id`+expiry+`)`, id, workspace, requester, action, kind, resource, hash, status).Scan(&valid)
	return valid, err
}
func (s approvalStore) AuthorityResources(ctx context.Context, id uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT workspace_id,resource_type,resource_id FROM approval_authority_resources WHERE approval_id=$1 ORDER BY resource_type,resource_id`, id)
}
