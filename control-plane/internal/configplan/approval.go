package configplan

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrApprovalNotReady = errors.New("configuration plan is not ready for approval")

// ApprovalBinding contains interpreted Plan content, not permission to approve
// or apply it. HTTP authorizes the node; Operations consumes approval atomically.
type ApprovalBinding struct {
	WorkspaceID, NodeID uuid.UUID
	RequestHash         []byte
	RequestSummary      json.RawMessage
}

func (s *Service) ApprovalBinding(ctx context.Context, id uuid.UUID) (ApprovalBinding, error) {
	plan, err := s.Get(ctx, id)
	if err != nil {
		return ApprovalBinding{}, err
	}
	now, err := value.FromTime(s.now())
	if err != nil {
		return ApprovalBinding{}, err
	}
	if plan.Validation != "valid" || !plan.ExpiresAt.Valid || plan.ExpiresAt.Micros <= now.Micros {
		return ApprovalBinding{}, ErrApprovalNotReady
	}
	// Keep the existing candidate hash and exact summary representation; this is
	// not a new hash of the summary and must remain compatible with stored approvals.
	hash, err := hex.DecodeString(plan.CandidateHash)
	if err != nil {
		return ApprovalBinding{}, err
	}
	summary, err := json.Marshal(map[string]any{"node_id": plan.NodeID, "expected_revision": plan.ExpectedRevision, "candidate_hash": plan.CandidateHash, "current_hash": plan.CurrentHash, "diff_redacted": plan.DiffRedacted, "expires_at": plan.ExpiresAt})
	if err != nil {
		return ApprovalBinding{}, err
	}
	if plan.MaterializedHash != "" {
		materialized, err := hex.DecodeString(plan.MaterializedHash)
		if err != nil {
			return ApprovalBinding{}, err
		}
		current, err := hex.DecodeString(plan.CurrentHash)
		if err != nil {
			return ApprovalBinding{}, err
		}
		expires, err := plan.ExpiresAt.Time()
		if err != nil {
			return ApprovalBinding{}, err
		}
		hash, err = configprofile.ApprovalHash(plan.ID, plan.NodeID, hash, materialized, current, uint64(plan.ExpectedRevision), timestamppb.New(expires))
		if err != nil {
			return ApprovalBinding{}, err
		}
		summary, err = json.Marshal(map[string]any{"plan_id": plan.ID, "node_id": plan.NodeID, "expected_revision": plan.ExpectedRevision, "candidate_hash": plan.CandidateHash, "materialized_hash": plan.MaterializedHash, "current_hash": plan.CurrentHash, "diff_redacted": plan.DiffRedacted, "expires_at": plan.ExpiresAt})
		if err != nil {
			return ApprovalBinding{}, err
		}
	}
	return ApprovalBinding{WorkspaceID: plan.WorkspaceID, NodeID: plan.NodeID, RequestHash: hash, RequestSummary: summary}, nil
}
