package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type approvalStore struct{ database.Tx }

func (t *transaction) ApprovalStore() approvalstore.Store { return approvalStore{t} }
func (s approvalStore) ConsumeBound(ctx context.Context, id, workspace, requester uuid.UUID, action, resourceType string, resource uuid.UUID, hash []byte) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE approval_requests SET status='consumed',consumed_at=now() WHERE id=$1 AND workspace_id=$2 AND requester_id=$3 AND action=$4 AND resource_type=$5 AND resource_id=$6 AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND approver_id IS DISTINCT FROM requester_id AND expires_at>now() AND request_hash=$7`, id, workspace, requester, action, resourceType, resource, hash)
	return n == 1, err
}
