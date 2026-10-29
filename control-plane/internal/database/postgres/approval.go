package postgres

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	// Lock the approval and its approver before reading the wall clock: either
	// lock wait may outlast the approval's expiry.
	var locked uuid.UUID
	if err := s.QueryRow(ctx, `SELECT id FROM approval_requests WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 FOR UPDATE`, id, workspace, requester).Scan(&locked); errors.Is(err, database.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if ok, err := s.approverAuthorized(ctx, id); err != nil || !ok {
		return false, err
	}
	now, err := database.WallTime(ctx, tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE approval_requests SET status='consumed',consumed_at=$8 WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6 AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND approver_id IS DISTINCT FROM requester_id AND expires_at>$8 AND request_hash=$7`, id, workspace, requester, action, resourceType, resource, hash, now)
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

// approverAuthorized re-evaluates the recorded approver's current authority.
func (s approvalStore) approverAuthorized(ctx context.Context, id uuid.UUID) (bool, error) {
	var approver *uuid.UUID
	if err := s.QueryRow(ctx, `SELECT approver_id FROM approval_requests WHERE id=$1`, id).Scan(&approver); errors.Is(err, database.ErrNotFound) || approver == nil {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return s.Authorized(ctx, id, *approver)
}
func (s approvalStore) Authorized(ctx context.Context, id, actor uuid.UUID) (bool, error) {
	// The share lock conflicts with the identity lock taken before disabling, so
	// a disable is either visible here or waits until this transaction ends.
	var active uuid.UUID
	if err := s.QueryRow(ctx, `SELECT id FROM identities WHERE id=$1 AND disabled_at IS NULL FOR SHARE`, actor).Scan(&active); errors.Is(err, database.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
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
	if err != nil || !valid || consumed {
		// A consumed approval stays verifiable after its approver is disabled.
		return valid, err
	}
	return s.approverAuthorized(ctx, id)
}
func (s approvalStore) AuthorityResources(ctx context.Context, id uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT workspace_id,resource_type,resource_id FROM approval_authority_resources WHERE approval_id=$1 ORDER BY resource_type,resource_id`, id)
}

func (s approvalStore) ListPending(ctx context.Context, workspace, actor, after uuid.UUID, limit int, at time.Time) (database.Rows, error) {
	return s.Query(ctx, `SELECT a.id,a.workspace_id,a.requester_id,a.approver_id,a.action,a.resource_type,a.resource_id,a.reason,a.status,a.expires_at,a.created_at FROM approval_requests a
    WHERE a.workspace_id=$1 AND a.status='pending' AND a.expires_at>$2 AND a.requester_id<>$3 AND a.id>$4
    AND EXISTS(SELECT 1 FROM approval_authority_resources WHERE approval_id=a.id)
    AND NOT EXISTS(SELECT 1 FROM approval_authority_resources scope WHERE scope.approval_id=a.id AND (scope.workspace_id<>a.workspace_id OR NOT EXISTS(
      SELECT 1 FROM role_bindings binding WHERE binding.identity_id=$3 AND binding.workspace_id=scope.workspace_id AND binding.created_at<=a.authority_snapshot_at
      AND binding.role_name IN ('SecurityAdmin','PlatformAdmin') AND (binding.resource_type='workspace' OR (binding.resource_type=scope.resource_type AND binding.resource_id=scope.resource_id)))))
    ORDER BY a.id ASC LIMIT $5`, workspace, at, actor, after, limit)
}
