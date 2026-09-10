package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/google/uuid"
)

type enrollmentStore struct{ database.Tx }

func (t *transaction) EnrollmentStore() enrollmentstore.EnrollmentStore { return enrollmentStore{t} }

func enrollmentTokenTable(bootstrap bool) string {
	if bootstrap {
		return "node_bootstrap_tokens"
	}
	return "enrollment_tokens"
}

func (s enrollmentStore) WorkspaceExists(ctx context.Context, id uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, id).Scan(&v)
	return
}

func (s enrollmentStore) InsertToken(ctx context.Context, v enrollmentstore.Token, bootstrap bool) error {
	if bootstrap {
		_, err := s.Exec(ctx, `INSERT INTO node_bootstrap_tokens(id,workspace_id,token_hash,expected_environment,expected_node_name,expires_at,created_by,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, v.ID, v.WorkspaceID, v.Hash, v.Environment, v.ExpectedName, v.ExpiresAt, v.CreatedBy, v.CreatedAt)
		return err
	}
	_, err := s.Exec(ctx, `INSERT INTO enrollment_tokens(id,workspace_id,token_hash,expected_environment,expected_node_name,expected_endpoint_id,expires_at,created_by,created_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, v.ID, v.WorkspaceID, v.Hash, v.Environment, v.ExpectedName, v.Endpoint, v.ExpiresAt, v.CreatedBy, v.CreatedAt)
	return err
}

func (s enrollmentStore) TokenByHash(ctx context.Context, hash []byte, bootstrap, lock bool) (v enrollmentstore.Token, err error) {
	endpoint := "expected_endpoint_id"
	if bootstrap {
		endpoint = "bound_endpoint_id"
	}
	q := `SELECT id,workspace_id,expected_environment,expected_node_name,` + endpoint + `,expires_at,consumed_at,consumed_node_id,created_at FROM ` + enrollmentTokenTable(bootstrap) + ` WHERE token_hash=$1`
	if lock {
		q += " FOR UPDATE"
	}
	err = s.QueryRow(ctx, q, hash).Scan(&v.ID, &v.WorkspaceID, &v.Environment, &v.ExpectedName, &v.Endpoint, &v.ExpiresAt, &v.ConsumedAt, &v.ConsumedNode, &v.CreatedAt)
	return
}

func (s enrollmentStore) ConsumeToken(ctx context.Context, token, node uuid.UUID, endpoint []byte, at value.Timestamp, bootstrap bool) (bool, error) {
	if bootstrap {
		n, err := s.Exec(ctx, `UPDATE node_bootstrap_tokens SET bound_endpoint_id=$1,consumed_node_id=$2,consumed_at=$3 WHERE id=$4 AND consumed_at IS NULL`, endpoint, node, at, token)
		return n == 1, err
	}
	n, err := s.Exec(ctx, `UPDATE enrollment_tokens SET consumed_at=$1,consumed_node_id=$2 WHERE id=$3 AND consumed_at IS NULL`, at, node, token)
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

func (s enrollmentStore) LegacyPendingNode(ctx context.Context, workspace uuid.UUID, name string) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT n.id FROM nodes n WHERE n.workspace_id=$1 AND n.name=$2 AND n.status='pending' AND NOT EXISTS(SELECT 1 FROM node_endpoint_keys k WHERE k.node_id=n.id) FOR UPDATE OF n`, workspace, name).Scan(&id)
	return
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

func (s enrollmentStore) TouchNode(ctx context.Context, id uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE nodes SET version=version+1,updated_at=$2 WHERE id=$1`, id, at)
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

func (s enrollmentStore) ResetCapabilities(ctx context.Context, id uuid.UUID) error {
	_, err := s.Exec(ctx, `UPDATE node_capabilities SET approved=false WHERE node_id=$1`, id)
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
