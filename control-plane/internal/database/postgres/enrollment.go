package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
)

type enrollmentStore struct{ database.Tx }

func (t *transaction) EnrollmentStore() enrollmentstore.EnrollmentStore { return enrollmentStore{t} }

func (s enrollmentStore) WorkspaceExists(ctx context.Context, id uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, id).Scan(&v)
	return
}

func (s enrollmentStore) InsertToken(ctx context.Context, v enrollmentstore.Token) error {
	_, err := s.Exec(ctx, `INSERT INTO node_bootstrap_tokens(id,workspace_id,token_hash,expected_environment,expected_node_name,expires_at,created_by,created_at,expected_endpoint_id)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, v.ID, v.WorkspaceID, v.Hash, v.Environment, v.ExpectedName, v.ExpiresAt, v.CreatedBy, v.CreatedAt, v.ExpectedEndpoint)
	return err
}

func (s enrollmentStore) TokenByHash(ctx context.Context, hash []byte, lock bool) (v enrollmentstore.Token, err error) {
	q := `SELECT id,workspace_id,expected_environment,expected_node_name,bound_endpoint_id,expires_at,consumed_at,consumed_node_id,created_at,expected_endpoint_id FROM node_bootstrap_tokens WHERE token_hash=$1`
	if lock {
		q += " FOR UPDATE"
	}
	err = s.QueryRow(ctx, q, hash).Scan(&v.ID, &v.WorkspaceID, &v.Environment, &v.ExpectedName, &v.Endpoint, &v.ExpiresAt, &v.ConsumedAt, &v.ConsumedNode, &v.CreatedAt, &v.ExpectedEndpoint)
	return
}

func (s enrollmentStore) ConsumeToken(ctx context.Context, token, node uuid.UUID, endpoint []byte, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE node_bootstrap_tokens SET bound_endpoint_id=$1,consumed_node_id=$2,consumed_at=$3 WHERE id=$4 AND consumed_at IS NULL`, endpoint, node, at, token)
	return n == 1, err
}

func enrollmentNodeLock(lock enrollmentstore.Lock) string {
	switch lock {
	case enrollmentstore.ForUpdate:
		return " FOR UPDATE OF n,k"
	case enrollmentstore.ForShare:
		return " FOR SHARE OF n,k"
	default:
		return ""
	}
}

const enrollmentNodeColumns = `SELECT n.id,n.workspace_id,n.name,n.status,k.state,k.endpoint_id,n.authorization_revision,n.version FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id `

func scanEnrollmentNode(row database.Row) (v enrollmentstore.Node, err error) {
	err = row.Scan(&v.ID, &v.WorkspaceID, &v.Name, &v.Status, &v.EndpointState, &v.Endpoint, &v.AuthorizationRevision, &v.Version)
	return
}

func (s enrollmentStore) NodeByEndpoint(ctx context.Context, endpoint []byte, workspace uuid.UUID, lock enrollmentstore.Lock) (enrollmentstore.Node, error) {
	var scope any
	if workspace != uuid.Nil {
		scope = workspace
	}
	return scanEnrollmentNode(s.QueryRow(ctx, enrollmentNodeColumns+`WHERE k.endpoint_id=$1 AND ($2::uuid IS NULL OR n.workspace_id=$2)`+enrollmentNodeLock(lock), endpoint, scope))
}

func (s enrollmentStore) NodeByID(ctx context.Context, id uuid.UUID, lock enrollmentstore.Lock) (enrollmentstore.Node, error) {
	return scanEnrollmentNode(s.QueryRow(ctx, enrollmentNodeColumns+`WHERE n.id=$1`+enrollmentNodeLock(lock), id))
}

func (s enrollmentStore) PendingCount(ctx context.Context, workspace uuid.UUID) (v int, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE workspace_id=$1 AND status='pending'`, workspace).Scan(&v)
	return
}

func (s enrollmentStore) EndpointExists(ctx context.Context, endpoint []byte) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_endpoint_keys WHERE endpoint_id=$1)`, endpoint).Scan(&v)
	return
}

func (s enrollmentStore) InsertNode(ctx context.Context, id, workspace uuid.UUID, name string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)VALUES($1,$2,$3,'pending',$4,$4)`, id, workspace, name, at)
	return err
}

func (s enrollmentStore) InsertEndpoint(ctx context.Context, id uuid.UUID, endpoint []byte, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at)VALUES($1,$2,'pending',$3)`, id, endpoint, at)
	return err
}

func (s enrollmentStore) InsertSealingKey(ctx context.Context, id uuid.UUID, key enrollmentstore.SealingKey, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at)VALUES($1,$2,$3,$4,$5,$6)`, id, key.Purpose, key.Version, key.ID, key.Digest, at)
	return err
}

func (s enrollmentStore) SealingKeys(ctx context.Context, id uuid.UUID) ([]enrollmentstore.SealingKey, error) {
	rows, err := s.Query(ctx, `SELECT purpose,version,key_id,public_key_sha256 FROM node_sealing_keys WHERE node_id=$1 ORDER BY purpose`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []enrollmentstore.SealingKey
	for rows.Next() {
		var key enrollmentstore.SealingKey
		if err := rows.Scan(&key.Purpose, &key.Version, &key.ID, &key.Digest); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s enrollmentStore) Capabilities(ctx context.Context, id uuid.UUID, approved bool) ([]string, error) {
	rows, err := s.Query(ctx, `SELECT capability FROM node_capabilities WHERE node_id=$1 AND (NOT $2::boolean OR approved) ORDER BY capability`, id, approved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var capabilities []string
	for rows.Next() {
		var capability string
		if err := rows.Scan(&capability); err != nil {
			return nil, err
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities, rows.Err()
}

func (s enrollmentStore) PutCapability(ctx context.Context, id uuid.UUID, capability string, approved bool) error {
	q := `INSERT INTO node_capabilities(node_id,capability,approved)VALUES($1,$2,$3)`
	if approved {
		q += ` ON CONFLICT(node_id,capability) DO UPDATE SET approved=true`
	}
	_, err := s.Exec(ctx, q, id, capability, approved)
	return err
}

func (s enrollmentStore) Activate(ctx context.Context, id uuid.UUID, labels, policy string, at value.Timestamp) (revision uint64, err error) {
	err = s.QueryRow(ctx, `UPDATE nodes SET status='active',labels=$2::jsonb,policy=$3,version=version+1,authorization_revision=authorization_revision+1,updated_at=$4 WHERE id=$1 RETURNING authorization_revision`, id, labels, policy, at).Scan(&revision)
	if err == nil {
		_, err = s.Exec(ctx, `UPDATE node_endpoint_keys SET state='active' WHERE node_id=$1`, id)
	}
	return
}

func (s enrollmentStore) Revoke(ctx context.Context, id uuid.UUID, at value.Timestamp) (revision uint64, err error) {
	err = s.QueryRow(ctx, `UPDATE nodes SET status='revoked',version=version+1,authorization_revision=authorization_revision+1,updated_at=$2 WHERE id=$1 RETURNING authorization_revision`, id, at).Scan(&revision)
	if err == nil {
		_, err = s.Exec(ctx, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=$2 WHERE node_id=$1`, id, at)
	}
	return
}

func (s enrollmentStore) EndpointPermitted(ctx context.Context, endpoint []byte, enroll bool) (v bool, err error) {
	q := `SELECT EXISTS(SELECT 1 FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE k.endpoint_id=$1 AND n.status IN ('active','offline') AND k.state='active')`
	if enroll {
		q = `SELECT NOT EXISTS(SELECT 1 FROM node_endpoint_keys WHERE endpoint_id=$1 AND state='revoked')`
	}
	err = s.QueryRow(ctx, q, endpoint).Scan(&v)
	return
}

func (s enrollmentStore) TrustSnapshot(ctx context.Context) ([]enrollmentstore.Node, error) {
	rows, err := s.Query(ctx, `SELECT n.id,k.endpoint_id,CASE WHEN n.status='revoked' OR k.state='revoked' THEN 'revoked' ELSE 'active' END,n.authorization_revision FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE (n.status IN ('active','offline') AND k.state='active') OR k.state='revoked' ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]enrollmentstore.Node, 0)
	for rows.Next() {
		var n enrollmentstore.Node
		if err := rows.Scan(&n.ID, &n.Endpoint, &n.Status, &n.AuthorizationRevision); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

type privdAttestationStore struct{ tx database.Tx }

func (t *transaction) PrivdAttestationStore() privdattestation.Store { return privdAttestationStore{t} }

func (s privdAttestationStore) KeyStateCounts(ctx context.Context) (database.Rows, error) {
	return s.tx.Query(ctx, `SELECT state,count(*) FROM node_privd_attestation_keys GROUP BY state`)
}

func (s privdAttestationStore) VerificationKey(ctx context.Context, node uuid.UUID, id string) (privdattestation.VerificationKey, error) {
	var k privdattestation.VerificationKey
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

func (s privdAttestationStore) InsertKey(ctx context.Context, k privdattestation.Key) error {
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

func (s privdAttestationStore) FirstKey(ctx context.Context, node uuid.UUID) (string, privdattestation.VerificationKey, error) {
	var id string
	var k privdattestation.VerificationKey
	err := s.tx.QueryRow(ctx, `SELECT key_id,public_key,state,activated_at,valid_until FROM node_privd_attestation_keys WHERE node_id=$1 ORDER BY created_at,key_id LIMIT 1`, node).Scan(&id, &k.PublicKey, &k.State, &k.ActivatedAt, &k.ValidUntil)
	return id, k, err
}

type trustConvergenceStore struct{ database.Tx }

func (t *transaction) TrustConvergenceStore() enrollmentstore.TrustStore {
	return trustConvergenceStore{t}
}

func (s trustConvergenceStore) Enqueue(ctx context.Context, v enrollmentstore.TrustJob, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO node_trust_convergence
		(node_id,endpoint_id,desired_state,revision,reason,close_required,available_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$3::text='revoked',$6,$6,$6)
		ON CONFLICT(node_id) DO UPDATE SET endpoint_id=EXCLUDED.endpoint_id,desired_state=EXCLUDED.desired_state,
		revision=EXCLUDED.revision,reason=EXCLUDED.reason,update_applied=false,close_required=EXCLUDED.close_required,
		close_applied=false,available_at=EXCLUDED.available_at,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=EXCLUDED.updated_at
		WHERE node_trust_convergence.revision < EXCLUDED.revision`, v.NodeID, v.EndpointID, v.DesiredState, v.Revision, v.Reason, at)
	return err
}

func (s trustConvergenceStore) Claim(ctx context.Context, worker uuid.UUID) (v enrollmentstore.TrustJob, err error) {
	at, err := database.WallTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	err = s.QueryRow(ctx, `SELECT node_id,endpoint_id,desired_state,revision,reason,update_applied,close_required,close_applied,attempts
		FROM node_trust_convergence WHERE (NOT update_applied OR (close_required AND NOT close_applied))
		AND available_at<=$1 AND (locked_until IS NULL OR locked_until<=$1)
		ORDER BY available_at,node_id FOR UPDATE SKIP LOCKED LIMIT 1`, at).Scan(
		&v.NodeID, &v.EndpointID, &v.DesiredState, &v.Revision, &v.Reason, &v.UpdateApplied, &v.CloseRequired, &v.CloseApplied, &v.Attempts)
	if err != nil {
		return v, err
	}
	at, err = database.WallTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return v, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=$2,locked_until=$4,attempts=attempts+1,updated_at=$5 WHERE node_id=$1 AND revision=$3`, v.NodeID, worker, v.Revision, until, at)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	v.Attempts++
	return v, err
}

// Check time after the row lock, including waits that cross the lease deadline.
func (s trustConvergenceStore) lockClaim(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (value.Timestamp, bool, error) {
	var until value.Timestamp
	err := s.QueryRow(ctx, `SELECT locked_until FROM node_trust_convergence WHERE node_id=$1 AND revision=$2 AND locked_by=$3 AND attempts=$4 FOR UPDATE`, v.NodeID, v.Revision, worker, v.Attempts).Scan(&until)
	if errors.Is(err, database.ErrNotFound) {
		return value.Timestamp{}, false, nil
	}
	if err != nil {
		return value.Timestamp{}, false, err
	}
	at, err := database.WallTime(ctx, s.Tx)
	return at, err == nil && until.Valid && until.Micros > at.Micros, err
}

func (s trustConvergenceStore) Renew(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_until=$2,updated_at=$3 WHERE node_id=$1`, v.NodeID, until, at)
	return err == nil, err
}

func (s trustConvergenceStore) MarkUpdateApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	until, err := at.Add(enrollmentstore.TrustLeaseTTL)
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,locked_until=$2,last_error=NULL,updated_at=$3 WHERE node_id=$1`, v.NodeID, until, at)
	return err == nil, err
}

func (s trustConvergenceStore) MarkCloseApplied(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) (bool, error) {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET close_applied=true,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=$2 WHERE node_id=$1 AND update_applied AND close_required`, v.NodeID, at)
	return n == 1, err
}

func (s trustConvergenceStore) UnlockComplete(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID) error {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=$2 WHERE node_id=$1 AND update_applied AND (NOT close_required OR close_applied)`, v.NodeID, at)
	return err
}

func (s trustConvergenceStore) Release(ctx context.Context, v enrollmentstore.TrustJob, worker uuid.UUID, delay time.Duration, detail string) error {
	at, valid, err := s.lockClaim(ctx, v, worker)
	if err != nil || !valid {
		return err
	}
	next, err := at.Add(delay)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE node_trust_convergence SET locked_by=NULL,locked_until=NULL,available_at=$2,last_error=$3,updated_at=$4 WHERE node_id=$1`, v.NodeID, next, detail, at)
	return err
}

func (s trustConvergenceStore) RecordSynchronized(ctx context.Context, v enrollmentstore.TrustJob, closed bool) (bool, error) {
	at, err := database.WallTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	n, err := s.Exec(ctx, `UPDATE node_trust_convergence SET update_applied=true,close_applied=close_applied OR (close_required AND $6),last_error=NULL,updated_at=$7
		WHERE node_id=$1 AND endpoint_id=$2 AND desired_state=$3 AND revision=$4 AND reason=$5`, v.NodeID, v.EndpointID, v.DesiredState, v.Revision, v.Reason, closed, at)
	return n == 1, err
}
