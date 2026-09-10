package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates/artifactstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"time"
)

type artifactStore struct{ database.Tx }

func (s artifactStore) Resource(ctx context.Context, id uuid.UUID) (workspace, node uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,node_id FROM artifact_operations WHERE id=$1`, id).Scan(&workspace, &node)
	return
}

func (t *transaction) ArtifactStore() artifactstore.Store { return artifactStore{t} }
func (s artifactStore) LockCapacity(ctx context.Context) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(6820260817)`)
	return err
}
func (s artifactStore) Active(ctx context.Context) (n int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM artifact_operations WHERE state='leased' AND lease_until>now()`).Scan(&n)
	return
}
func (s artifactStore) Eligible(ctx context.Context, id uuid.UUID, hash []byte) (v artifactstore.Eligible, err error) {
	err = s.QueryRow(ctx, `SELECT a.node_id,a.certificate_id,a.operation_id,a.certificate_version,a.content_sha256,a.content_size,a.expires_at,c.not_after,r.requester_id FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id JOIN approval_requests r ON r.id=a.approval_id WHERE a.id=$1 AND a.token_sha256=$2 AND a.expires_at>now() AND a.certificate_version=c.version AND c.not_after>now() AND c.state IN ('issued','expiring') AND (a.state='ready' OR (a.state='leased' AND a.lease_until<now())) FOR UPDATE OF a`, id, hash).Scan(&v.NodeID, &v.CertificateID, &v.OperationID, &v.CertificateVersion, &v.Digest, &v.Size, &v.ArtifactExpires, &v.CertificateExpires, &v.RequesterID)
	return
}
func (s artifactStore) Lease(ctx context.Context, id, grant, subject uuid.UUID, expires time.Time) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state='leased',lease_until=$2,active_grant_id=$3,active_grant_subject=$4,active_grant_expires_at=$2,updated_at=now() WHERE id=$1`, id, expires, grant, subject.String())
	return err
}
func (s artifactStore) StartConsumption(ctx context.Context, v artifactstore.Consumption) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE artifact_operations a SET state='consuming',consume_grant=$6,consume_sha256=$3,consume_size=$4,consume_actor_id=$7,consume_session_id=$8,consume_request_id=$9,updated_at=now() FROM certificates c WHERE a.id=$1 AND a.active_grant_id=$2 AND a.active_grant_subject=$5 AND a.state='leased' AND a.content_sha256=$3 AND a.content_size=$4 AND a.expires_at>now() AND a.certificate_id=c.id AND a.node_id=$10 AND a.certificate_id=$11 AND a.certificate_version=$12 AND a.operation_id=$13 AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>now()`, v.ID, v.GrantID, v.Digest, v.Size, v.ActorID.String(), v.Grant, v.ActorID, v.SessionID, v.RequestID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID)
	return n == 1, err
}
func (s artifactStore) ExactReplay(ctx context.Context, v artifactstore.Consumption) (replay bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id WHERE a.id=$1 AND a.active_grant_id=$2 AND a.state='consumed' AND a.consume_grant=$3 AND a.consume_sha256=$4 AND a.consume_size=$5 AND a.consume_actor_id=$6 AND a.consume_session_id=$7 AND a.consume_request_id=$8 AND a.node_id=$9 AND a.certificate_id=$10 AND a.certificate_version=$11 AND a.operation_id=$12 AND a.certificate_version=c.version AND c.state IN ('issued','expiring') AND c.not_after>now())`, v.ID, v.GrantID, v.Grant, v.Digest, v.Size, v.ActorID, v.SessionID, v.RequestID, v.NodeID, v.CertificateID, v.CertificateVersion, v.OperationID).Scan(&replay)
	return
}
func (s artifactStore) Finalize(ctx context.Context, id, grant uuid.UUID) (v artifactstore.Finalized, err error) {
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
	state := "ready"
	if expired {
		state = "expired"
	}
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state=$3,lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming'`, id, grant, state)
	return err
}
