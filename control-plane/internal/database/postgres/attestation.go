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
	_, err := s.tx.Exec(ctx, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,predecessor_key_id) VALUES($1,$2,'ed25519',$3,'active',$4,$4,$4,$5)`,
		k.NodeID, k.KeyID, k.PublicKey, k.CreatedAt, k.PredecessorKeyID)
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

func (s privdAttestationStore) FirstKey(ctx context.Context, node uuid.UUID) (string, attestationstore.VerificationKey, error) {
	var id string
	var k attestationstore.VerificationKey
	err := s.tx.QueryRow(ctx, `SELECT key_id,public_key,state,activated_at,valid_until FROM node_privd_attestation_keys WHERE node_id=$1 ORDER BY created_at,key_id LIMIT 1`, node).Scan(&id, &k.PublicKey, &k.State, &k.ActivatedAt, &k.ValidUntil)
	return id, k, err
}
