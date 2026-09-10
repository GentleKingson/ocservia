package mysql

import (
	"context"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func (s certificateStore) OperationExists(ctx context.Context, workspace uuid.UUID, key string) (exists bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=? AND BINARY idempotency_key=?)`, UUIDBytes(workspace), key).Scan(&exists)
	return
}

func (s certificateStore) BeginRevocation(ctx context.Context, id uuid.UUID, at time.Time) error {
	stamp, err := value.FromTime(at)
	if err != nil {
		return err
	}
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE certificates SET state='revoking',version=version+1,updated_at=? WHERE id=? AND state IN ('issued','expiring','expired','revocation_unknown')`, stamp, UUIDBytes(id)); err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE artifact_operations SET state='revoked',lease_until=NULL,active_grant_expires_at=?,updated_at=? WHERE certificate_id=? AND state IN ('pending','ready','leased','consuming')`, now, now, UUIDBytes(id))
	return err
}

func (s certificateStore) RevocationUnknown(ctx context.Context, id uuid.UUID, at time.Time) error {
	stamp, err := value.FromTime(at)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE certificates SET state='revocation_unknown',version=version+1,updated_at=? WHERE id=? AND state='revoking'`, stamp, UUIDBytes(id))
	return err
}

func (s certificateStore) ReleaseOperation(ctx context.Context, id uuid.UUID) error {
	// The operation dispatcher must poll this durable row; pg_notify is only
	// a PostgreSQL latency hint, not delivery authority.
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE outbox_events SET available_at=? WHERE command_id=(SELECT command_id FROM operations WHERE id=?) AND published_at IS NULL`, now, UUIDBytes(id))
	return err
}

func (s certificateStore) P12Eligible(ctx context.Context, id uuid.UUID) (v certificatestore.ActionBinding, err error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,certificate_chain_pem,serial_number,version FROM certificates WHERE id=? AND state IN ('issued','expiring') AND not_after>?`, UUIDBytes(id), now).Scan(&v.WorkspaceID, &v.NodeID, &v.Chain, &v.Serial, &v.Version)
	return
}

func (s certificateStore) ArtifactByRequest(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v certificatestore.Artifact, err error) {
	err = s.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=? FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=? AND BINARY o.idempotency_key=?`, hash, UUIDBytes(workspace), key).Scan(&v.ID, &v.OperationID, &v.ExpiresAt, &v.SameIntent)
	return
}

func (s certificateStore) ArtifactByOperation(ctx context.Context, id uuid.UUID) (v certificatestore.Artifact, err error) {
	err = s.QueryRow(ctx, `SELECT id,operation_id,expires_at FROM artifact_operations WHERE operation_id=?`, UUIDBytes(id)).Scan(&v.ID, &v.OperationID, &v.ExpiresAt)
	return
}

func (s certificateStore) SealingKeyExists(ctx context.Context, node uuid.UUID, version uint32, key string) (exists bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sealing_keys WHERE node_id=? AND purpose=2 AND version=? AND BINARY key_id=?)`, UUIDBytes(node), version, key).Scan(&exists)
	return
}
