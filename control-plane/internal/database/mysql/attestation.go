package mysql

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
	"github.com/google/uuid"
)

type privdAttestationStore struct{ tx database.Tx }

func (t *transaction) PrivdAttestationStore() attestationstore.Store { return privdAttestationStore{t} }

// attestationTime converts a business-finite instant to the checked
// PostgreSQL-epoch microsecond representation of the converted columns.
func attestationTime(t time.Time) (value.Timestamp, error) {
	v, err := value.FromTime(t)
	if err != nil {
		return v, err
	}
	return v, v.Validate()
}

func (s privdAttestationStore) LockActiveNode(ctx context.Context, node uuid.UUID) (uuid.UUID, error) {
	var workspace []byte
	if err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=? AND status IN ('active','offline') FOR UPDATE`, UUIDBytes(node)).Scan(&workspace); err != nil {
		return uuid.UUID{}, err
	}
	return uuid.FromBytes(workspace)
}

func (s privdAttestationStore) LockNode(ctx context.Context, node uuid.UUID) (uuid.UUID, error) {
	var workspace []byte
	if err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(node)).Scan(&workspace); err != nil {
		return uuid.UUID{}, err
	}
	return uuid.FromBytes(workspace)
}

func (s privdAttestationStore) OutstandingCredential(ctx context.Context, node uuid.UUID, now time.Time) (bool, error) {
	at, err := attestationTime(now)
	if err != nil {
		return false, err
	}
	var outstanding bool
	err = s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM privd_attestation_enrollment_credentials WHERE node_id=? AND consumed_at IS NULL AND expires_at>?)`, UUIDBytes(node), at).Scan(&outstanding)
	return outstanding, err
}

func (s privdAttestationStore) InsertCredential(ctx context.Context, c attestationstore.Credential) error {
	expires, err := attestationTime(c.ExpiresAt)
	if err != nil {
		return err
	}
	created, err := attestationTime(c.CreatedAt)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,created_by_identity_id,created_by_session_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		UUIDBytes(c.ID), UUIDBytes(c.NodeID), c.SecretSHA256, c.ControllerNonce, c.ContextSHA256, expires, UUIDBytes(c.CreatedByIdentityID), UUIDBytes(c.CreatedBySessionID), created)
	return err
}

func (s privdAttestationStore) LockCredential(ctx context.Context, secretDigest []byte) (attestationstore.Credential, error) {
	var c attestationstore.Credential
	var id, node []byte
	var expires, consumed value.Timestamp
	err := s.tx.QueryRow(ctx, `SELECT id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at FROM privd_attestation_enrollment_credentials WHERE secret_sha256=? FOR UPDATE`, secretDigest).
		Scan(&id, &node, &c.SecretSHA256, &c.ControllerNonce, &c.ContextSHA256, &expires, &consumed)
	if err != nil {
		return c, err
	}
	if c.ExpiresAt, err = expires.Time(); err != nil {
		return c, err
	}
	if consumed.Valid {
		consumedAt, err := consumed.Time()
		if err != nil {
			return c, err
		}
		c.ConsumedAt = &consumedAt
	}
	if c.ID, err = uuid.FromBytes(id); err != nil {
		return c, err
	}
	c.NodeID, err = uuid.FromBytes(node)
	return c, err
}

func (s privdAttestationStore) ActiveKeys(ctx context.Context, node uuid.UUID, now time.Time) ([]string, error) {
	at, err := attestationTime(now)
	if err != nil {
		return nil, err
	}
	rows, err := s.tx.Query(ctx, `SELECT key_id FROM node_privd_attestation_keys WHERE node_id=? AND state='active' AND (valid_until IS NULL OR valid_until>?) ORDER BY activated_at FOR UPDATE`, UUIDBytes(node), at)
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
	created, err := attestationTime(k.CreatedAt)
	if err != nil {
		return err
	}
	var predecessor any
	if k.PredecessorKeyID != nil {
		predecessor = *k.PredecessorKeyID
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,predecessor_key_id,registration_credential_id) VALUES(?,?,'ed25519',?,'active',?,?,?,?,?)`,
		UUIDBytes(k.NodeID), k.KeyID, k.PublicKey, created, created, created, predecessor, UUIDBytes(k.RegistrationCredentialID))
	return err
}

func (s privdAttestationStore) RotatePredecessor(ctx context.Context, node uuid.UUID, predecessor, successor string, validUntil time.Time) error {
	until, err := attestationTime(validUntil)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET valid_until=?,successor_key_id=? WHERE node_id=? AND key_id=? AND state='active'`, until, successor, UUIDBytes(node), predecessor)
	return err
}

func (s privdAttestationStore) ConsumeCredential(ctx context.Context, id uuid.UUID, now time.Time) error {
	at, err := attestationTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE privd_attestation_enrollment_credentials SET consumed_at=? WHERE id=? AND consumed_at IS NULL`, at, UUIDBytes(id))
	return err
}

func (s privdAttestationStore) ApproveCapability(ctx context.Context, node uuid.UUID, capability string) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,?,true) ON DUPLICATE KEY UPDATE approved=true`, UUIDBytes(node), capability)
	return err
}

func (s privdAttestationStore) BumpNodeRevision(ctx context.Context, node uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE nodes SET authorization_revision=authorization_revision+1,version=version+1,updated_at=? WHERE id=?`, now, UUIDBytes(node))
	return err
}

func (s privdAttestationStore) RevokeKey(ctx context.Context, node uuid.UUID, keyID string, now time.Time) (bool, error) {
	at, err := attestationTime(now)
	if err != nil {
		return false, err
	}
	affected, err := s.tx.Exec(ctx, `UPDATE node_privd_attestation_keys SET state='revoked',revoked_at=?,valid_until=LEAST(COALESCE(valid_until,?),?) WHERE node_id=? AND CAST(key_id AS BINARY)=CAST(? AS BINARY) AND state='active'`, at, at, at, UUIDBytes(node), keyID)
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
