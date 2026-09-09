package mysql

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type approvalStore struct{ database.Tx }

func (t *transaction) ApprovalStore() approvalstore.Store { return approvalStore{t} }
func (s approvalStore) ConsumeBound(ctx context.Context, id, workspace, requester uuid.UUID, action, resourceType string, resource uuid.UUID, hash []byte) (bool, error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE approval_requests SET status='consumed',consumed_at=? WHERE id=? AND workspace_id=? AND requester_id=? AND CAST(action AS BINARY)=CAST(? AS BINARY) AND CAST(resource_type AS BINARY)=CAST(? AS BINARY) AND resource_id=? AND status='approved' AND request_hash IS NOT NULL AND request_summary IS NOT NULL AND NOT(approver_id <=> requester_id) AND expires_at>? AND request_hash=?`, now, UUIDBytes(id), UUIDBytes(workspace), UUIDBytes(requester), action, resourceType, UUIDBytes(resource), now, hash)
	return n == 1, err
}
