package mysql

import (
	"context"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
)

func (s certificateStore) InsertCSR(ctx context.Context, v certificatestore.PendingCSR) error {
	_, err := s.Exec(ctx, `INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,'csr_pending',?,?)`, UUIDBytes(v.ID), UUIDBytes(v.WorkspaceID), UUIDBytes(v.NodeID), UUIDBytes(v.OperationID), v.CommonName, v.DNSNames, v.KeyBits, v.CreatedAt, v.CreatedAt)
	return err
}

func (s certificateStore) InsertArtifact(ctx context.Context, v certificatestore.PendingArtifact) error {
	_, err := s.Exec(ctx, `INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at,approval_id)
		VALUES(?,?,?,?,?,?,'certificate_p12','pending',?,?,?,?,?,?)`, UUIDBytes(v.ID), UUIDBytes(v.WorkspaceID), UUIDBytes(v.NodeID), UUIDBytes(v.CertificateID), v.CertificateVersion, UUIDBytes(v.OperationID), v.TokenSHA256, v.RequestHash, v.ExpiresAt, v.CreatedAt, v.CreatedAt, UUIDBytes(v.ApprovalID))
	return err
}
