package postgres

import (
	"context"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type certificateStore struct{ database.Tx }

func (t *transaction) CertificateStore() certificatestore.Store { return certificateStore{t} }

func (s certificateStore) Maintain(ctx context.Context, at time.Time) error {
	for _, state := range []string{"expired", "expiring"} {
		predicate := `state IN ('issued','expiring') AND not_after<=now()`
		if state == "expiring" {
			predicate = `state='issued' AND not_after>now() AND not_after<=now()+interval '30 days'`
		}
		rows, err := s.Query(ctx, `WITH due AS (
			SELECT id FROM certificates WHERE `+predicate+` ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED
		) UPDATE certificates c SET state=$1,version=version+1,updated_at=now() FROM due WHERE c.id=due.id RETURNING c.id,c.workspace_id,c.node_id`, state)
		if err != nil {
			return err
		}
		type alert struct{ id, workspace, node uuid.UUID }
		var alerts []alert
		for rows.Next() {
			var a alert
			if err := rows.Scan(&a.id, &a.workspace, &a.node); err != nil {
				rows.Close()
				return err
			}
			alerts = append(alerts, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, a := range alerts {
			if _, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at) VALUES($1,$2,'high',$3,$4,'certificate',$5,$6)`, uuid.Must(uuid.NewV7()), a.workspace, "certificate."+state, a.node, a.id, at); err != nil {
				return err
			}
		}
		if state == "expired" {
			if _, err := s.Exec(ctx, `UPDATE artifact_operations a SET state='expired',lease_until=NULL,updated_at=now() FROM certificates c WHERE c.id=a.certificate_id AND c.state='expired' AND a.state IN ('pending','ready','leased','consuming')`); err != nil {
				return err
			}
		}
	}
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,updated_at=now() WHERE state IN ('pending','ready','leased') AND expires_at<=now()`)
	return err
}

func (s certificateStore) InsertCSR(ctx context.Context, v certificatestore.PendingCSR) error {
	_, err := s.Exec(ctx, `INSERT INTO certificates(id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'csr_pending',$8,$8)`, v.ID, v.WorkspaceID, v.NodeID, v.OperationID, v.CommonName, v.DNSNames, v.KeyBits, v.CreatedAt)
	return err
}

func (s certificateStore) InsertArtifact(ctx context.Context, v certificatestore.PendingArtifact) error {
	_, err := s.Exec(ctx, `INSERT INTO artifact_operations(id,workspace_id,node_id,certificate_id,certificate_version,operation_id,purpose,state,token_sha256,request_hash,expires_at,created_at,updated_at,requester_identity_id)
		VALUES($1,$2,$3,$4,$5,$6,'certificate_p12','pending',$7,$8,$9,$10,$10,$11)`, v.ID, v.WorkspaceID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID, v.TokenSHA256, v.RequestHash, v.ExpiresAt, v.CreatedAt, v.RequesterID)
	return err
}

func (s certificateStore) LockIssue(ctx context.Context, id uuid.UUID) (v certificatestore.IssueState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,state,issue_request_hash,issue_actor_identity_id,version,issue_certificate_version,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy),csr_receipt_sha256,csr_privd_attestation_key_id,csr_der_sha256,common_name,dns_names,key_bits FROM certificates WHERE id=$1 FOR UPDATE`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.CSR, &v.State, &v.RequestHash, &v.ActorID, &v.Version, &v.IssueVersion, &v.ReceiptCurrent, &v.ReceiptDigest, &v.ReceiptKey, &v.CSRDigest, &v.CommonName, &v.DNSNames, &v.KeyBits)
	return
}

func (s certificateStore) SigningReceipt(ctx context.Context, id uuid.UUID) (v certificatestore.IssueReceipt, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,csr_der,csr_privd_attestation_key_id,csr_receipt_sha256,csr_der_sha256,issue_request_hash,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy),EXISTS(SELECT 1 FROM node_privd_attestation_keys k WHERE k.node_id=certificates.node_id AND k.key_id=certificates.csr_privd_attestation_key_id AND k.state='active' AND (k.valid_until IS NULL OR k.valid_until>now())) FROM certificates WHERE id=$1 AND state='signing'`, id).Scan(&v.NodeID, &v.CSR, &v.ReceiptKey, &v.ReceiptDigest, &v.CSRDigest, &v.RequestHash, &v.Current, &v.KeyActive)
	return
}

func (s certificateStore) LockReceipt(ctx context.Context, id uuid.UUID) (v certificatestore.IssueReceipt, err error) {
	err = s.QueryRow(ctx, `SELECT node_id,csr_der,csr_privd_attestation_key_id,csr_receipt_sha256,csr_der_sha256,issue_request_hash,(csr_receipt_verified_at IS NOT NULL AND NOT csr_receipt_legacy) FROM certificates WHERE id=$1 FOR UPDATE`, id).Scan(&v.NodeID, &v.CSR, &v.ReceiptKey, &v.ReceiptDigest, &v.CSRDigest, &v.RequestHash, &v.Current)
	return
}

func (s certificateStore) KeyActive(ctx context.Context, node uuid.UUID, key string, at time.Time) (active bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$2 AND state='active' AND (valid_until IS NULL OR valid_until>$3))`, node, key, at).Scan(&active)
	return
}

func (s certificateStore) StartIssue(ctx context.Context, v certificatestore.Signing) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,issue_request_hash=$2,issue_actor_identity_id=$3,issue_certificate_version=$4,updated_at=$5 WHERE id=$1`, v.ID, v.RequestHash, v.ActorID, v.Version, v.At)
	return err
}

func (s certificateStore) ResumeIssue(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,updated_at=$2 WHERE id=$1`, id, at)
	return err
}

func (s certificateStore) SignerUnavailable(ctx context.Context, id uuid.UUID, hash []byte, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='signer_unavailable',version=version+1,updated_at=$2 WHERE id=$1 AND state='signing' AND issue_request_hash=$3`, id, at, hash)
	return err
}

func (s certificateStore) CompleteIssue(ctx context.Context, v certificatestore.Issued) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE certificates SET state='issued',version=version+1,certificate_chain_pem=$2,serial_number=$3,not_before=$4,not_after=$5,updated_at=$6 WHERE id=$1 AND state IN ('signing','signer_unavailable') AND issue_request_hash=$7`, v.ID, v.Chain, v.Serial, v.NotBefore, v.NotAfter, v.At, v.RequestHash)
	return n == 1, err
}

const certificateColumns = `id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at`

func scanCertificate(row database.Row) (v certificatestore.Certificate, err error) {
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.NodeID, &v.OperationID, &v.CommonName, &v.DNSNames, &v.KeyBits, &v.State, &v.Version, &v.PublicKeySHA256, &v.SerialNumber, &v.NotBefore, &v.NotAfter, &v.RevokedAt, &v.CreatedAt, &v.UpdatedAt)
	return
}

func (s certificateStore) Get(ctx context.Context, id uuid.UUID) (certificatestore.Certificate, error) {
	return scanCertificate(s.QueryRow(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE id=$1`, id))
}

func (s certificateStore) GetByOperation(ctx context.Context, id uuid.UUID) (certificatestore.Certificate, error) {
	return scanCertificate(s.QueryRow(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE operation_id=$1`, id))
}

func (s certificateStore) ListNode(ctx context.Context, id uuid.UUID) ([]certificatestore.Certificate, error) {
	rows, err := s.Query(ctx, `SELECT `+certificateColumns+` FROM certificates WHERE node_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]certificatestore.Certificate, 0)
	for rows.Next() {
		v, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s certificateStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM certificates WHERE id=$1`, id).Scan(&workspace, &node)
	return
}

const secretReferenceColumns = `id,workspace_id,provider,key_path,version,state,rotated_at,created_at,updated_at`

func scanSecretReference(row database.Row) (v certificatestore.SecretReference, err error) {
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.Provider, &v.KeyPath, &v.Version, &v.State, &v.RotatedAt, &v.CreatedAt, &v.UpdatedAt)
	return
}

func (s certificateStore) InsertSecretReference(ctx context.Context, v certificatestore.SecretReference) error {
	_, err := s.Exec(ctx, `INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at)VALUES($1,$2,$3,$4,$5,'active',$6,$7)`, v.ID, v.WorkspaceID, v.Provider, v.KeyPath, v.Version, v.CreatedAt, v.UpdatedAt)
	return err
}

func (s certificateStore) RotateSecretReference(ctx context.Context, id uuid.UUID, version string, at time.Time) (certificatestore.SecretReference, error) {
	return scanSecretReference(s.QueryRow(ctx, `UPDATE secret_provider_refs SET version=$2,state='active',rotated_at=$3,updated_at=$3 WHERE id=$1 RETURNING `+secretReferenceColumns, id, version, at))
}

func (s certificateStore) GetSecretReference(ctx context.Context, id uuid.UUID) (certificatestore.SecretReference, error) {
	return scanSecretReference(s.QueryRow(ctx, `SELECT `+secretReferenceColumns+` FROM secret_provider_refs WHERE id=$1`, id))
}

func (s certificateStore) SecretReferenceResource(ctx context.Context, id uuid.UUID) (workspace uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM secret_provider_refs WHERE id=$1`, id).Scan(&workspace)
	return
}

func (s certificateStore) OperationExists(ctx context.Context, workspace uuid.UUID, key string) (exists bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$1 AND idempotency_key=$2)`, workspace, key).Scan(&exists)
	return
}

func (s certificateStore) BeginRevocation(ctx context.Context, id uuid.UUID, at time.Time) error {
	if _, err := s.Exec(ctx, `UPDATE certificates SET state='revoking',version=version+1,updated_at=$2 WHERE id=$1 AND state IN ('issued','expiring','expired','revocation_unknown')`, id, at); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state='revoked',lease_until=NULL,active_grant_expires_at=now(),updated_at=now() WHERE certificate_id=$1 AND state IN ('pending','ready','leased','consuming')`, id)
	return err
}

func (s certificateStore) RevocationUnknown(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state='revocation_unknown',version=version+1,updated_at=$2 WHERE id=$1 AND state='revoking'`, id, at)
	return err
}

func (s certificateStore) ReleaseOperation(ctx context.Context, id uuid.UUID) error {
	_, err := s.Exec(ctx, `UPDATE outbox_events SET available_at=now() WHERE command_id=(SELECT command_id FROM operations WHERE id=$1) AND published_at IS NULL`, id)
	return err
}

func (s certificateStore) P12Eligible(ctx context.Context, id uuid.UUID) (v certificatestore.ActionBinding, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,certificate_chain_pem,serial_number,version FROM certificates WHERE id=$1 AND state IN ('issued','expiring') AND not_after>now()`, id).Scan(&v.WorkspaceID, &v.NodeID, &v.Chain, &v.Serial, &v.Version)
	return
}

func (s certificateStore) ArtifactByRequest(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v certificatestore.Artifact, err error) {
	err = s.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=$3 FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspace, key, hash).Scan(&v.ID, &v.OperationID, &v.ExpiresAt, &v.SameIntent)
	return
}

func (s certificateStore) ArtifactByOperation(ctx context.Context, id uuid.UUID) (v certificatestore.Artifact, err error) {
	err = s.QueryRow(ctx, `SELECT id,operation_id,expires_at FROM artifact_operations WHERE operation_id=$1`, id).Scan(&v.ID, &v.OperationID, &v.ExpiresAt)
	return
}

func (s certificateStore) SealingKeyExists(ctx context.Context, node uuid.UUID, version uint32, key string) (exists bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sealing_keys WHERE node_id=$1 AND purpose=2 AND version=$2 AND key_id=$3)`, node, version, key).Scan(&exists)
	return
}

type artifactStore struct{ database.Tx }

func (s artifactStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM artifact_operations WHERE id=$1`, id).Scan(&workspace, &node)
	return
}

func (t *transaction) ArtifactStore() certificatestore.ArtifactStore { return artifactStore{t} }
func (s artifactStore) LockCapacity(ctx context.Context) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(6820260817)`)
	return err
}
func (s artifactStore) Active(ctx context.Context) (n int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM artifact_operations WHERE state='leased' AND lease_until>now()`).Scan(&n)
	return
}
func (s artifactStore) Eligible(ctx context.Context, id uuid.UUID, hash []byte) (v certificatestore.Eligible, err error) {
	err = s.QueryRow(ctx, `SELECT a.node_id,a.certificate_id,a.operation_id,a.certificate_version,a.content_sha256,a.content_size,a.expires_at,c.not_after,a.requester_identity_id FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id WHERE a.id=$1 AND a.token_sha256=$2 AND a.expires_at>now() AND a.certificate_version=c.version AND c.not_after>now() AND c.state IN ('issued','expiring') AND (a.state='ready' OR (a.state='leased' AND a.lease_until<now())) FOR UPDATE OF a`, id, hash).Scan(&v.NodeID, &v.CertificateID, &v.OperationID, &v.CertificateVersion, &v.Digest, &v.Size, &v.ArtifactExpires, &v.CertificateExpires, &v.RequesterID)
	return
}
func (s artifactStore) Lease(ctx context.Context, id, grant, subject uuid.UUID, expires time.Time) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state='leased',lease_until=$2,active_grant_id=$3,active_grant_subject=$4,active_grant_expires_at=$2,updated_at=now() WHERE id=$1`, id, expires, grant, subject.String())
	return err
}
func (s artifactStore) StartConsumption(ctx context.Context, v certificatestore.Consumption) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE artifact_operations a SET state='consuming',consume_grant=$6,consume_sha256=$3,consume_size=$4,consume_actor_id=$7,consume_session_id=$8,consume_request_id=$9,updated_at=now() FROM certificates c WHERE a.id=$1 AND a.active_grant_id=$2 AND a.active_grant_subject=$5 AND a.state='leased' AND a.content_sha256=$3 AND a.content_size=$4 AND a.expires_at>now() AND a.certificate_id=c.id AND a.node_id=$10 AND a.certificate_id=$11 AND a.certificate_version=$12 AND a.operation_id=$13 AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>now()`, v.ID, v.GrantID, v.Digest, v.Size, v.ActorID.String(), v.Grant, v.ActorID, v.SessionID, v.RequestID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID)
	return n == 1, err
}
func (s artifactStore) ExactReplay(ctx context.Context, v certificatestore.Consumption) (replay bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id WHERE a.id=$1 AND a.active_grant_id=$2 AND a.state='consumed' AND a.consume_grant=$3 AND a.consume_sha256=$4 AND a.consume_size=$5 AND a.consume_actor_id=$6 AND a.consume_session_id=$7 AND a.consume_request_id=$8 AND a.node_id=$9 AND a.certificate_id=$10 AND a.certificate_version=$11 AND a.operation_id=$12 AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>now())`, v.ID, v.GrantID, v.Grant, v.Digest, v.Size, v.ActorID, v.SessionID, v.RequestID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID).Scan(&replay)
	return
}
func (s artifactStore) Finalize(ctx context.Context, id, grant uuid.UUID) (v certificatestore.Finalized, err error) {
	err = s.QueryRow(ctx, `UPDATE artifact_operations SET state='consumed',consumed_at=now(),lease_until=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming' RETURNING workspace_id,node_id,certificate_id,consume_actor_id,consume_session_id,consume_request_id,consume_sha256,consume_size`, id, grant).Scan(&v.WorkspaceID, &v.NodeID, &v.CertificateID, &v.ActorID, &v.SessionID, &v.RequestID, &v.Digest, &v.Size)
	return
}
func (s artifactStore) State(ctx context.Context, id, grant uuid.UUID) (state string, err error) {
	err = s.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1 AND active_grant_id=$2`, id, grant).Scan(&state)
	return
}
func (s artifactStore) Abort(ctx context.Context, id, grant uuid.UUID) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='leased'`, id, grant)
	return err
}

func (s artifactStore) PendingConsumptions(ctx context.Context) ([]certificatestore.PendingConsumption, error) {
	rows, err := s.Query(ctx, `SELECT id,active_grant_id,consume_grant,consume_sha256,consume_size,consume_actor_id,expires_at FROM artifact_operations WHERE state='consuming' ORDER BY updated_at,id LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]certificatestore.PendingConsumption, 0, 20)
	for rows.Next() {
		var v certificatestore.PendingConsumption
		if err := rows.Scan(&v.ID, &v.GrantID, &v.Grant, &v.Digest, &v.Size, &v.ActorID, &v.ExpiresAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s artifactStore) ResetConsumption(ctx context.Context, id, grant uuid.UUID, expired bool) error {
	state := "ready"
	if expired {
		state = "expired"
	}
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state=$3,lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming'`, id, grant, state)
	return err
}
