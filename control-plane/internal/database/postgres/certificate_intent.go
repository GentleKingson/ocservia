package postgres

import (
	"context"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
)

func (s certificateStore) InsertCSR(ctx context.Context, v certificatestore.PendingCSR) error {
	_, err := s.Exec(ctx, `INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'csr_pending',$8,$8)`, v.ID, v.WorkspaceID, v.NodeID, v.OperationID, v.CommonName, v.DNSNames, v.KeyBits, v.CreatedAt)
	return err
}

func (s certificateStore) InsertArtifact(ctx context.Context, v certificatestore.PendingArtifact) error {
	_, err := s.Exec(ctx, `INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at,approval_id)
		VALUES($1,$2,$3,$4,$5,$6,'certificate_p12','pending',$7,$8,$9,$10,$10,$11)`, v.ID, v.WorkspaceID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID, v.TokenSHA256, v.RequestHash, v.ExpiresAt, v.CreatedAt, v.ApprovalID)
	return err
}
