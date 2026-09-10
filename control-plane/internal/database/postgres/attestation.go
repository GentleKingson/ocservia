package postgres

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
	"github.com/google/uuid"
)

type privdAttestationStore struct{ tx database.Tx }

func (t *transaction) PrivdAttestationStore() attestationstore.Store { return privdAttestationStore{t} }

func (s privdAttestationStore) KeyStateCounts(ctx context.Context) (database.Rows, error) {
	return s.tx.Query(ctx, `SELECT state,count(*) FROM node_privd_attestation_keys GROUP BY state`)
}

func (s privdAttestationStore) VerificationKey(ctx context.Context, node uuid.UUID, id string) (attestationstore.VerificationKey, error) {
	var k attestationstore.VerificationKey
	err := s.tx.QueryRow(ctx, `SELECT public_key,state,activated_at,valid_until FROM node_privd_attestation_keys WHERE node_id=$1 AND key_id=$2`, node, id).Scan(&k.PublicKey, &k.State, &k.ActivatedAt, &k.ValidUntil)
	return k, err
}

func (s privdAttestationStore) LockActiveNode(ctx context.Context, node uuid.UUID) (uuid.UUID, error) {
	var workspace uuid.UUID
	err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1 AND status IN ('active','offline') FOR UPDATE`, node).Scan(&workspace)
	return workspace, err
}

func (s privdAttestationStore) LockNode(ctx context.Context, node uuid.UUID) (uuid.UUID, error) {
	var workspace uuid.UUID
	err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1 FOR UPDATE`, node).Scan(&workspace)
	return workspace, err
}

func (s privdAttestationStore) OutstandingCredential(ctx context.Context, node uuid.UUID, now time.Time) (bool, error) {
	var outstanding bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM privd_attestation_enrollment_credentials WHERE node_id=$1 AND consumed_at IS NULL AND expires_at>$2)`, node, now).Scan(&outstanding)
	return outstanding, err
}

func (s privdAttestationStore) InsertCredential(ctx context.Context, c attestationstore.Credential) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,created_by_identity_id,created_by_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		c.ID, c.NodeID, c.SecretSHA256, c.ControllerNonce, c.ContextSHA256, c.ExpiresAt, c.CreatedByIdentityID, c.CreatedBySessionID, c.CreatedAt)
	return err
}

func (s privdAttestationStore) LockCredential(ctx context.Context, secretDigest []byte) (attestationstore.Credential, error) {
	var c attestationstore.Credential
	err := s.tx.QueryRow(ctx, `SELECT id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at FROM privd_attestation_enrollment_credentials WHERE secret_sha256=$1 FOR UPDATE`, secretDigest).
		Scan(&c.ID, &c.NodeID, &c.SecretSHA256, &c.ControllerNonce, &c.ContextSHA256, &c.ExpiresAt, &c.ConsumedAt)
	return c, err
}

func (s privdAttestationStore) ActiveKeys(ctx context.Context, node uuid.UUID, now time.Time) ([]string, error) {
	rows, err := s.tx.Query(ctx, `SELECT key_id FROM node_privd_attestation_keys WHERE node_id=$1 AND state='active' AND (valid_until IS NULL OR valid_until>$2) ORDER BY activated_at FOR UPDATE`, node, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var active []string
	for rows.Next() {
		var keyID string
		if err = rows.Scan(&keyID); err != nil {
			return nil, err
		}
		active = append(active, keyID)
	}
	return active, rows.Err()
}

func (s privdAttestationStore) InsertKey(ctx context.Context, k attestationstore.Key) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,predecessor_key_id,registration_credential_id) VALUES($1,$2,'ed25519',$3,'active',$4,$4,$4,$5,$6)`,
		k.NodeID, k.KeyID, k.PublicKey, k.CreatedAt, k.PredecessorKeyID, k.RegistrationCredentialID)
	return err
}

func (s privdAttestationStore) RotatePredecessor(ctx context.Context, node uuid.UUID, predecessor, successor string, validUntil time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET valid_until=$3,successor_key_id=$2 WHERE node_id=$1 AND key_id=$4 AND state='active'`, node, successor, validUntil, predecessor)
	return err
}

func (s privdAttestationStore) ConsumeCredential(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE privd_attestation_enrollment_credentials SET consumed_at=$2 WHERE id=$1 AND consumed_at IS NULL`, id, now)
	return err
}

func (s privdAttestationStore) ApproveCapability(ctx context.Context, node uuid.UUID, capability string) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true) ON CONFLICT(node_id,capability) DO UPDATE SET approved=true`, node, capability)
	return err
}

func (s privdAttestationStore) BumpNodeRevision(ctx context.Context, node uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE nodes SET authorization_revision=authorization_revision+1,version=version+1,updated_at=$2 WHERE id=$1`, node, now)
	return err
}

func (s privdAttestationStore) RevokeKey(ctx context.Context, node uuid.UUID, keyID string, now time.Time) (bool, error) {
	affected, err := s.tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET state='revoked',revoked_at=$3,valid_until=LEAST(COALESCE(valid_until,$3),$3) WHERE node_id=$1 AND key_id=$2 AND state='active'`, node, keyID, now)
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
