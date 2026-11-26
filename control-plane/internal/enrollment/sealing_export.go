package enrollment

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

// ExportSealingBinding is a local administrative operation. Its caller must
// authenticate to the Controller database, never pass those credentials to Signer.
func ExportSealingBinding(ctx context.Context, backend database.Backend, workspace, nodeID uuid.UUID, endpoint string) ([]byte, error) {
	var output []byte
	err := database.Within(ctx, backend, database.RepeatableRead, func(tx database.Tx) error {
		store := enrollmentstore.Enrollment(tx)
		node, err := store.NodeByID(ctx, nodeID, enrollmentstore.ForShare)
		if err != nil {
			return err
		}
		if node.WorkspaceID != workspace || (node.Status != "active" && node.Status != "offline") || node.EndpointState != "active" || hex.EncodeToString(node.Endpoint) != endpoint || len(node.Endpoint) != 32 {
			return ErrInvalidTransition
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
		output, err = json.Marshal(map[string]any{"workspace_id": workspace, "node_id": nodeID, "endpoint_id": endpoint, "exported_at": time.Now().UTC(), "keys": descriptors})
		return err
	})
	return output, err
}
