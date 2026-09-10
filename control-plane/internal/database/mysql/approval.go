package mysql

import (
	"context"
	"encoding/json"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"time"
)

type approvalStore struct{ database.Store }

func (b *Backend) ApprovalStore() approvalstore.Store     { return approvalStore{b} }
func (t *transaction) ApprovalStore() approvalstore.Store { return approvalStore{t} }
func (s approvalStore) ConsumeBound(ctx context.Context, id, workspace, requester uuid.UUID, action, resourceType string, resource uuid.UUID, hash []byte) (bool, error) {
	tx, ok := s.Store.(database.Tx)
	if !ok {
		return false, database.ErrUnsupported
	}
	now, err := database.TransactionTime(ctx, tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE approval_requests SET status='consumed',consumed_at=? WHERE id=? AND workspace_id=? AND requester_id=? AND CAST(action AS BINARY)=CAST(? AS BINARY) AND CAST(resource_type AS BINARY)=CAST(? AS BINARY) AND resource_id=? AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND NOT(approver_id <=> requester_id) AND expires_at>? AND request_hash=?`, now, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(requester), action, resourceType, UUIDBytes(resource), now, hash)
	return n == 1, err
}

func (s approvalStore) Insert(ctx context.Context, args []any) error {
	args = append([]any(nil), args...)
	for i, arg := range args {
		if id, ok := arg.(uuid.UUID); ok {
			args[i] = UUIDBytes(id)
		}
	}
	summary, err := value.ParseJSONB(args[10].(json.RawMessage))
	if err != nil {
		return err
	}
	args[10] = summary
	args = append(args, args[8])
	_, err = s.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,expires_at,created_at,request_hash,request_summary,authority_snapshot_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?,?,?)`, args...)
	return err
}
func (s approvalStore) AddAuthority(ctx context.Context, id, workspace uuid.UUID, kind string, resource uuid.UUID) error {
	// The newly inserted approval row belongs exclusively to this transaction.
	var exists bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_authority_resources WHERE approval_id=? AND workspace_id=? AND CAST(resource_type AS BINARY)=CAST(? AS BINARY) AND resource_id=?)`, UUIDBytes(id), UUIDBytes(workspace), kind, UUIDBytes(resource)).Scan(&exists)
	if err != nil || exists {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES(?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), kind, UUIDBytes(resource))
	return err
}
func (s approvalStore) AddBatchItem(ctx context.Context, id uuid.UUID, index int, node uuid.UUID, username, action string, version int64) error {
	_, err := s.Exec(ctx, `INSERT INTO approval_batch_items(approval_id,item_index,node_id,username,action,expected_version) VALUES(?,?,?,?,?,?)`, UUIDBytes(id), index, UUIDBytes(node), username, action, version)
	return err
}
func (s approvalStore) Get(ctx context.Context, id uuid.UUID, lock bool) database.Row {
	q := `SELECT id,workspace_id,requester_id,approver_id,action,resource_type,resource_id,reason,status,expires_at,created_at,COALESCE(LOWER(HEX(request_hash)),''),request_summary FROM approval_requests WHERE id=?`
	if lock {
		q += ` FOR UPDATE`
	}
	return s.QueryRow(ctx, q, UUIDBytes(id))
}
func (s approvalStore) Authorized(ctx context.Context, id, actor uuid.UUID) (bool, error) {
	var valid bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_authority_resources WHERE approval_id=?) AND NOT EXISTS (SELECT 1 FROM approval_authority_resources scope WHERE scope.approval_id=? AND NOT EXISTS (SELECT 1 FROM role_bindings binding WHERE binding.identity_id=? AND binding.workspace_id=scope.workspace_id AND binding.created_at <= (SELECT authority_snapshot_at FROM approval_requests WHERE id=?) AND binding.role_name IN ('SecurityAdmin','PlatformAdmin') AND (binding.resource_type='workspace' OR (CAST(binding.resource_type AS BINARY)=CAST(scope.resource_type AS BINARY) AND binding.resource_id=scope.resource_id))))`, UUIDBytes(id), UUIDBytes(id), UUIDBytes(actor), UUIDBytes(id)).Scan(&valid)
	return valid, err
}
func (s approvalStore) Approve(ctx context.Context, id, actor uuid.UUID, reason string, at time.Time) error {
	stamp, err := value.FromTime(at)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE approval_requests SET status='approved',approver_id=?,approval_reason=?,approved_at=? WHERE id=?`, UUIDBytes(actor), reason, stamp, UUIDBytes(id))
	return err
}
func (s approvalStore) ValidBound(ctx context.Context, id, workspace, requester uuid.UUID, action, kind string, resource uuid.UUID, hash []byte, consumed bool) (bool, error) {
	status, expiry := "approved", ` AND expires_at>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))`
	if consumed {
		status, expiry = "consumed", ""
	}
	var valid bool
	err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_requests WHERE id=? AND workspace_id=? AND requester_id=? AND CAST(action AS BINARY)=CAST(? AS BINARY) AND CAST(resource_type AS BINARY)=CAST(? AS BINARY) AND resource_id=? AND request_hash=? AND status=? AND NOT(approver_id <=> requester_id)`+expiry+`)`, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(requester), action, kind, UUIDBytes(resource), hash, status).Scan(&valid)
	return valid, err
}
func (s approvalStore) AuthorityResources(ctx context.Context, id uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT workspace_id,resource_type,resource_id FROM approval_authority_resources WHERE approval_id=? ORDER BY resource_type,resource_id`, UUIDBytes(id))
}
