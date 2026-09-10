package mysql

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates/artifactstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"time"
)

type artifactStore struct{ database.Tx }

func (s artifactStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM artifact_operations WHERE id=?`, UUIDBytes(id)).Scan(&workspace, &node)
	return
}

func (t *transaction) ArtifactStore() artifactstore.Store { return artifactStore{t} }
func (s artifactStore) clock(ctx context.Context) (value.Timestamp, error) {
	return database.TransactionTime(ctx, s.Tx)
}
func (s artifactStore) LockCapacity(ctx context.Context) error {
	return LockTransaction(ctx, s.Tx, "pg-advisory:6820260817")
}
func (s artifactStore) Active(ctx context.Context) (n int, err error) {
	now, err := s.clock(ctx)
	if err != nil {
		return
	}
	err = s.QueryRow(ctx, `SELECT count(*) FROM artifact_operations WHERE state='leased' AND lease_until>?`, now).Scan(&n)
	return
}
func (s artifactStore) Eligible(ctx context.Context, id uuid.UUID, hash []byte) (v artifactstore.Eligible, err error) {
	now, err := s.clock(ctx)
	if err != nil {
		return
	}
	// Lock only the artifact, matching PostgreSQL FOR UPDATE OF a. Joining
	// locking tables here would also lock approvals/certificates in InnoDB.
	var locked []byte
	if err = s.QueryRow(ctx, `SELECT id FROM artifact_operations WHERE id=? FOR UPDATE`, UUIDBytes(id)).Scan(&locked); err != nil {
		return
	}
	err = s.QueryRow(ctx, `SELECT a.node_id,a.certificate_id,a.operation_id,a.certificate_version,a.content_sha256,a.content_size,a.expires_at,c.not_after,r.requester_id FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id JOIN approval_requests r ON r.id=a.approval_id WHERE a.id=? AND a.token_sha256=? AND a.expires_at>? AND a.certificate_version=c.version AND c.not_after>? AND c.state IN ('issued','expiring') AND (a.state='ready' OR (a.state='leased' AND a.lease_until<?))`, UUIDBytes(id), hash, now, now, now).Scan(&v.NodeID, &v.CertificateID, &v.OperationID, &v.CertificateVersion, &v.Digest, &v.Size, &v.ArtifactExpires, &v.CertificateExpires, &v.RequesterID)
	return
}
func (s artifactStore) Lease(ctx context.Context, id, grant, subject uuid.UUID, expires time.Time) error {
	expiration, err := value.FromTime(expires)
	if err != nil {
		return err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE artifact_operations SET state='leased',lease_until=?,active_grant_id=?,active_grant_subject=?,active_grant_expires_at=?,updated_at=? WHERE id=?`, expiration, UUIDBytes(grant), subject.String(), expiration, now, UUIDBytes(id))
	return err
}
func (s artifactStore) StartConsumption(ctx context.Context, v artifactstore.Consumption) (bool, error) {
	now, err := s.clock(ctx)
	if err != nil {
		return false, err
	}
	// Keep the original target-row lock scope: a joined UPDATE would lock
	// certificate rows too. READ COMMITTED reads the certificate after locking a.
	var locked []byte
	if err = s.QueryRow(ctx, `SELECT id FROM artifact_operations WHERE id=? FOR UPDATE`, UUIDBytes(v.ID)).Scan(&locked); errors.Is(err, database.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var eligible bool
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_operations a JOIN certificates c ON a.certificate_id=c.id WHERE a.id=? AND a.active_grant_id=? AND BINARY a.active_grant_subject=? AND a.state='leased' AND a.content_sha256=? AND a.content_size=? AND a.expires_at>? AND a.node_id=? AND a.certificate_id=? AND a.certificate_version=? AND a.operation_id=? AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>?)`, UUIDBytes(v.ID), UUIDBytes(v.GrantID), v.ActorID.String(), v.Digest, v.Size, now, UUIDBytes(v.NodeID), UUIDBytes(v.CertificateID), v.CertificateVersion, UUIDBytes(v.OperationID), now).Scan(&eligible)
	if err != nil || !eligible {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE artifact_operations SET state='consuming',consume_grant=?,consume_sha256=?,consume_size=?,consume_actor_id=?,consume_session_id=?,consume_request_id=?,updated_at=? WHERE id=?`, v.Grant, v.Digest, v.Size, UUIDBytes(v.ActorID), UUIDBytes(v.SessionID), v.RequestID, now, UUIDBytes(v.ID))
	return n == 1, err
}
func (s artifactStore) ExactReplay(ctx context.Context, v artifactstore.Consumption) (replay bool, err error) {
	now, err := s.clock(ctx)
	if err != nil {
		return
	}
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id WHERE a.id=? AND a.active_grant_id=? AND a.state='consumed' AND a.consume_grant=? AND a.consume_sha256=? AND a.consume_size=? AND a.consume_actor_id=? AND a.consume_session_id=? AND BINARY a.consume_request_id=? AND a.node_id=? AND a.certificate_id=? AND a.certificate_version=? AND a.operation_id=? AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>?)`, UUIDBytes(v.ID), UUIDBytes(v.GrantID), v.Grant, v.Digest, v.Size, UUIDBytes(v.ActorID), UUIDBytes(v.SessionID), v.RequestID, UUIDBytes(v.NodeID), UUIDBytes(v.CertificateID), v.CertificateVersion, UUIDBytes(v.OperationID), now).Scan(&replay)
	return
}
func (s artifactStore) Finalize(ctx context.Context, id, grant uuid.UUID) (v artifactstore.Finalized, err error) {
	now, err := s.clock(ctx)
	if err != nil {
		return
	}
	n, err := s.Exec(ctx, `UPDATE artifact_operations SET state='consumed',consumed_at=?,lease_until=NULL,updated_at=? WHERE id=? AND active_grant_id=? AND state='consuming'`, now, now, UUIDBytes(id), UUIDBytes(grant))
	if err != nil {
		return
	}
	if n != 1 {
		return v, database.ErrNotFound
	}
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id,certificate_id,consume_actor_id,consume_session_id,consume_request_id,consume_sha256,consume_size FROM artifact_operations WHERE id=?`, UUIDBytes(id)).Scan(&v.WorkspaceID, &v.NodeID, &v.CertificateID, &v.ActorID, &v.SessionID, &v.RequestID, &v.Digest, &v.Size)
	return
}
func (s artifactStore) State(ctx context.Context, id, grant uuid.UUID) (state string, err error) {
	err = s.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=? AND active_grant_id=?`, UUIDBytes(id), UUIDBytes(grant)).Scan(&state)
	return
}
func (s artifactStore) Abort(ctx context.Context, id, grant uuid.UUID) error {
	now, err := s.clock(ctx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE artifact_operations SET updated_at=? WHERE id=? AND active_grant_id=? AND state='leased'`, now, UUIDBytes(id), UUIDBytes(grant))
	return err
}

func (s artifactStore) PendingConsumptions(ctx context.Context) ([]artifactstore.PendingConsumption, error) {
	rows, err := s.Query(ctx, `SELECT id,active_grant_id,consume_grant,consume_sha256,consume_size,consume_actor_id,expires_at FROM artifact_operations WHERE state='consuming' ORDER BY updated_at,id LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]artifactstore.PendingConsumption, 0, 20)
	for rows.Next() {
		var v artifactstore.PendingConsumption
		if err := rows.Scan(&v.ID, &v.GrantID, &v.Grant, &v.Digest, &v.Size, &v.ActorID, &v.ExpiresAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s artifactStore) ResetConsumption(ctx context.Context, id, grant uuid.UUID, expired bool) error {
	now, err := s.clock(ctx)
	if err != nil {
		return err
	}
	state := "ready"
	if expired {
		state = "expired"
	}
	_, err = s.Exec(ctx, `UPDATE artifact_operations SET state=?,lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,updated_at=? WHERE id=? AND active_grant_id=? AND state='consuming'`, state, now, UUIDBytes(id), UUIDBytes(grant))
	return err
}
