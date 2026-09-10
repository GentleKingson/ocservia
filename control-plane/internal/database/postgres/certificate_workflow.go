package postgres

import (
	"context"
	"errors"
	"time"

	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

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
	var outbox uuid.UUID
	err := s.QueryRow(ctx, `UPDATE outbox_events SET available_at=now() WHERE command_id=(SELECT command_id FROM operations WHERE id=$1) AND published_at IS NULL RETURNING id`, id).Scan(&outbox)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, outbox.String())
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
