package enrollment

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals/approvalstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

// ExportSealingBinding is a local administrative operation. Its caller must
// authenticate to the Controller database, never pass those credentials to Signer.
func ExportSealingBinding(ctx context.Context, backend database.Backend, workspace, nodeID, approvalID uuid.UUID, endpoint string) ([]byte, error) {
	var output []byte
	err := database.Within(ctx, backend, database.RepeatableRead, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		node, err := store.NodeByID(ctx, nodeID, enrollmentstore.ForShare)
		if err != nil {
			return err
		}
		if node.WorkspaceID != workspace || (node.Status != "active" && node.Status != "offline") || node.EndpointState != "active" || hex.EncodeToString(node.Endpoint) != endpoint || len(node.Endpoint) != 32 {
			return ErrInvalidTransition
		}
		provider, ok := tx.(approvalstore.Provider)
		if !ok {
			return database.ErrUnsupported
		}
		var approved approvals.Approval
		err = provider.ApprovalStore().Get(ctx, approvalID, false).Scan(&approved.ID, &approved.WorkspaceID, &approved.RequesterID, &approved.ApproverID, &approved.Action, &approved.ResourceType, &approved.ResourceID, &approved.Reason, &approved.Status, &approved.ExpiresAt, &approved.CreatedAt, &approved.RequestHash, &approved.RequestSummary)
		if err != nil {
			return err
		}
		var summary struct {
			NodeID       uuid.UUID   `json:"node_id"`
			EndpointID   string      `json:"endpoint_id"`
			NodeVersion  int64       `json:"node_version"`
			Policy       string      `json:"policy"`
			Labels       [][2]string `json:"labels"`
			Capabilities []string    `json:"capabilities"`
		}
		if json.Unmarshal(approved.RequestSummary, &summary) != nil || summary.NodeID != nodeID || summary.EndpointID != endpoint || summary.NodeVersion < 1 || approved.ApproverID == nil || *approved.ApproverID == approved.RequesterID {
			return ErrInvalidRequest
		}
		labels := map[string]string{}
		for _, v := range summary.Labels {
			labels[v[0]] = v[1]
		}
		hash, _, err := nodeApprovalBinding(ctx, store, nodeID, node.Endpoint, summary.NodeVersion, labels, summary.Policy, summary.Capabilities)
		if err != nil {
			return err
		}
		if err := approvals.ValidateConsumedBoundTx(ctx, tx, approvalID, workspace, approved.RequesterID, "node.approve", "node", nodeID, hash); err != nil {
			return err
		}
		keys, err := store.SealingKeys(ctx, nodeID)
		if err != nil {
			return err
		}
		if len(keys) != 2 || keys[0].Purpose == keys[1].Purpose || keys[0].ID == keys[1].ID || hex.EncodeToString(keys[0].Digest) == hex.EncodeToString(keys[1].Digest) {
			return ErrInvalidRequest
		}
		var descriptors []map[string]any
		for _, k := range keys {
			use := ""
			switch k.Purpose {
			case 1:
				use = "user_password"
			case 2:
				use = "certificate_p12_password"
			default:
				return ErrInvalidRequest
			}
			if k.Version != 1 || k.ID == "" || len(k.ID) > 128 || len(k.Digest) != 32 {
				return ErrInvalidRequest
			}
			descriptors = append(descriptors, map[string]any{"purpose": use, "version": k.Version, "key_id": k.ID, "public_key_sha256": hex.EncodeToString(k.Digest)})
		}
		output, err = json.Marshal(map[string]any{"workspace_id": workspace, "node_id": nodeID, "endpoint_id": endpoint, "approval_id": approvalID, "approval_hash": hex.EncodeToString(hash), "exported_at": time.Now().UTC(), "keys": descriptors})
		return err
	})
	return output, err
}
