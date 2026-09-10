package postgres

import (
	"context"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func (s certificateStore) LockIssue(ctx context.Context, id uuid.UUID) (v certificatestore.IssueState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,state,issue_approval_id,issue_request_hash,issue_actor_identity_id,version,issue_certificate_version,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy),csr_receipt_sha256,csr_privd_attestation_key_id,csr_der_sha256,csr_requested_subject_sha256,common_name,dns_names,key_bits FROM certificates WHERE id=$1 FOR UPDATE`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.CSR, &v.State, &v.ApprovalID, &v.RequestHash, &v.ActorID, &v.Version, &v.IssueVersion, &v.ReceiptCurrent, &v.ReceiptDigest, &v.ReceiptKey, &v.CSRDigest, &v.SubjectDigest, &v.CommonName, &v.DNSNames, &v.KeyBits)
	return
}

func (s certificateStore) ApprovedReceipt(ctx context.Context, id, approval uuid.UUID) (v certificatestore.IssueReceipt, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,csr_der,csr_privd_attestation_key_id,csr_receipt_sha256,csr_der_sha256,csr_requested_subject_sha256,issue_request_hash,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy),EXISTS(SELECT 1 FROM node_privd_attestation_keys k WHERE k.node_id=certificates.node_id AND k.key_id=certificates.csr_privd_attestation_key_id AND k.state='active' AND (k.valid_until IS NULL OR k.valid_until>now())) FROM certificates WHERE id=$1 AND state='signing' AND issue_approval_id=$2`, id, approval).Scan(&v.NodeID, &v.CSR, &v.ReceiptKey, &v.ReceiptDigest, &v.CSRDigest, &v.SubjectDigest, &v.RequestHash, &v.Current, &v.KeyActive)
	return
}

func (s certificateStore) LockReceipt(ctx context.Context, id uuid.UUID) (v certificatestore.IssueReceipt, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,csr_der,csr_privd_attestation_key_id,csr_receipt_sha256,csr_der_sha256,csr_requested_subject_sha256,issue_request_hash,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy) FROM certificates WHERE id=$1 FOR UPDATE`, id).Scan(&v.NodeID, &v.CSR, &v.ReceiptKey, &v.ReceiptDigest, &v.CSRDigest, &v.SubjectDigest, &v.RequestHash, &v.Current)
	return
}

func (s certificateStore) KeyActive(ctx context.Context, node uuid.UUID, key string, at value.Timestamp) (active bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$2 AND state='active' AND (valid_until IS NULL OR valid_until>$3))`, node, key, at).Scan(&active)
	return
}

func (s certificateStore) StartIssue(ctx context.Context, v certificatestore.Signing) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,issue_approval_id=$2,issue_request_hash=$3,issue_actor_identity_id=$4,issue_certificate_version=$5,updated_at=$6 WHERE id=$1`, v.ID, v.ApprovalID, v.RequestHash, v.ActorID, v.Version, v.At)
	return err
}

func (s certificateStore) ResumeIssue(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,updated_at=$2 WHERE id=$1`, id, at)
	return err
}

func (s certificateStore) SignerUnavailable(ctx context.Context, id, approval uuid.UUID, hash []byte, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signer_unavailable',version=version+1,updated_at=$2 WHERE id=$1 AND state='signing' AND issue_approval_id=$3 AND issue_request_hash=$4`, id, at, approval, hash)
	return err
}

func (s certificateStore) CompleteIssue(ctx context.Context, v certificatestore.Issued) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE certificates SET state='issued',version=version+1,certificate_chain_pem=$2,serial_number=$3,not_before=$4,not_after=$5,updated_at=$6 WHERE id=$1 AND state IN ('signing','signer_unavailable') AND issue_approval_id=$7 AND issue_request_hash=$8`, v.ID, v.Chain, v.Serial, v.NotBefore, v.NotAfter, v.At, v.ApprovalID, v.RequestHash)
	return n == 1, err
}

func (s certificateStore) IssueBinding(ctx context.Context, id uuid.UUID) (v certificatestore.IssueState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,common_name,dns_names,version,csr_receipt_sha256 FROM certificates WHERE id=$1 AND state='csr_ready' AND csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.CSR, &v.CommonName, &v.DNSNames, &v.Version, &v.ReceiptDigest)
	return
}

func (s certificateStore) ActionBinding(ctx context.Context, id uuid.UUID) (v certificatestore.ActionBinding, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,version,COALESCE(serial_number,''),COALESCE(certificate_chain_pem,''::bytea) FROM certificates WHERE id=$1`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.Version, &v.Serial, &v.Chain)
	return
}
