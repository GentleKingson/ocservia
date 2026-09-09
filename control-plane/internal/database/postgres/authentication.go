package postgres

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type authenticationStore struct{ tx database.Tx }

func (s authenticationStore) LockManagement(ctx context.Context) error {
	_, err := s.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734821032)`)
	return err
}
func (s authenticationStore) WorkspaceExists(ctx context.Context, id uuid.UUID) error {
	return s.tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR KEY SHARE`, id).Scan(&id)
}
func (s authenticationStore) Bootstrap(ctx context.Context) (authstore.Bootstrap, error) {
	var b authstore.Bootstrap
	err := s.tx.QueryRow(ctx, `SELECT identity_id,workspace_id,completion_pending FROM local_auth_bootstrap WHERE singleton FOR UPDATE`).Scan(&b.IdentityID, &b.WorkspaceID, &b.Pending)
	return b, err
}
func (s authenticationStore) ValidatePendingCredential(ctx context.Context, id, workspace uuid.UUID, hash string) error {
	return s.tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username AND i.disabled_at IS NULL AND c.password_hash=$2 AND EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=i.id AND workspace_id=$3 AND resource_type='workspace' AND role_name='PlatformAdmin') AND NOT EXISTS(SELECT 1 FROM role_bindings WHERE workspace_id=$3 AND identity_id<>i.id AND role_name IN ('SecurityAdmin','PlatformAdmin')) FOR UPDATE OF i,c`, id, hash, workspace).Scan(&id)
}
func (s authenticationStore) Initialized(ctx context.Context) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_bootstrap) OR EXISTS(SELECT 1 FROM identities i JOIN role_bindings b ON b.identity_id=i.id WHERE i.issuer='local' AND b.role_name IN ('SecurityAdmin','PlatformAdmin'))`).Scan(&yes)
	return yes, err
}
func (s authenticationStore) BindRole(ctx context.Context, id, identity, workspace uuid.UUID, role string, now time.Time) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,$4,'workspace',$5)`, id, identity, workspace, role, now)
	return err
}
func (s authenticationStore) InsertBootstrap(ctx context.Context, id, workspace uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO local_auth_bootstrap(singleton,identity_id,workspace_id,created_at,completed_at) VALUES(true,$1,$2,$3,$3)`, id, workspace, now)
	return err
}
func (s authenticationStore) CompleteBootstrap(ctx context.Context, approver uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE local_auth_bootstrap SET completion_pending=false,completed_at=$1,approver_identity_id=$2 WHERE singleton`, now, approver)
	return err
}
func (s authenticationStore) MayManage(ctx context.Context, actor, workspace, session uuid.UUID) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2 AND role_name='PlatformAdmin' AND resource_type='workspace') OR EXISTS(SELECT 1 FROM auth_sessions WHERE id=$3 AND break_glass)`, actor, workspace, session).Scan(&yes)
	return yes, err
}
func (s authenticationStore) LockLocalIdentity(ctx context.Context, id uuid.UUID) error {
	return s.tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username FOR UPDATE OF i,c`, id).Scan(&id)
}
func (s authenticationStore) Protected(ctx context.Context, workspace, id uuid.UUID) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `WITH effective AS (SELECT DISTINCT i.id,b.role_name FROM identities i JOIN local_credentials c ON c.identity_id=i.id JOIN role_bindings b ON b.identity_id=i.id WHERE i.disabled_at IS NULL AND i.issuer='local' AND i.subject=c.username AND b.workspace_id=$1 AND b.resource_type='workspace' AND b.role_name IN ('PlatformAdmin','SecurityAdmin')) SELECT EXISTS(SELECT 1 FROM effective WHERE id=$2) AND (NOT EXISTS(SELECT 1 FROM effective WHERE id<>$2 AND role_name='PlatformAdmin') OR (SELECT count(DISTINCT id) FROM effective WHERE id<>$2)<2)`, workspace, id).Scan(&yes)
	return yes, err
}
func (s authenticationStore) Disable(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE identities SET disabled_at=COALESCE(disabled_at,$2),updated_at=$2 WHERE id=$1`, id, now)
	return err
}
func (s authenticationStore) DeleteIdentityAttempts(ctx context.Context, id uuid.UUID) error {
	_, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=(SELECT username FROM local_credentials WHERE identity_id=$1)`, id)
	return err
}
func (s authenticationStore) BreakGlassUsed(ctx context.Context, fingerprint []byte) (bool, error) {
	var used bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM break_glass_uses WHERE credential_fingerprint=$1 AND rotation_required=true)`, fingerprint).Scan(&used)
	return used, err
}
func (s authenticationStore) BreakGlassIdentity(ctx context.Context, id uuid.UUID, now time.Time) (uuid.UUID, error) {
	err := s.tx.QueryRow(ctx, `INSERT INTO identities(id,issuer,subject,display_name,created_at,updated_at) VALUES($1,'break-glass','offline','Break-glass',$2,$2) ON CONFLICT(issuer,subject) DO UPDATE SET updated_at=EXCLUDED.updated_at RETURNING id`, id, now).Scan(&id)
	return id, err
}
func (s authenticationStore) RecordBreakGlass(ctx context.Context, fingerprint []byte, id, session, alert uuid.UUID, now time.Time) error {
	if _, err := s.tx.Exec(ctx, `INSERT INTO break_glass_uses(credential_fingerprint,identity_id,used_at,source_session_id,rotation_required) VALUES($1,$2,$3,$4,true)`, fingerprint, id, now, session); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO security_alerts(id,severity,kind,source_session_id,created_at) VALUES($1,'critical','break_glass.used',$2,$3)`, alert, session, now)
	return err
}
func (s authenticationStore) Workspaces(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.tx.Query(ctx, `SELECT id FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s authenticationStore) ActiveLocalUsername(ctx context.Context, id uuid.UUID) (string, error) {
	var username string
	err := s.tx.QueryRow(ctx, `SELECT c.username FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username AND i.disabled_at IS NULL`, id).Scan(&username)
	return username, err
}
func (s authenticationStore) LockActiveSession(ctx context.Context, identity, session uuid.UUID, local bool) error {
	var id uuid.UUID
	if err := s.tx.QueryRow(ctx, `SELECT id FROM identities WHERE id=$1 AND disabled_at IS NULL AND (NOT $2 OR issuer='local') FOR UPDATE`, identity, local).Scan(&id); err != nil {
		return err
	}
	return s.tx.QueryRow(ctx, `SELECT id FROM auth_sessions WHERE id=$1 AND identity_id=$2 AND revoked_at IS NULL AND expires_at>clock_timestamp() AND (NOT $3 OR NOT break_glass) FOR UPDATE`, session, identity, local).Scan(&id)
}
func (s authenticationStore) LockPassword(ctx context.Context, id uuid.UUID, username, hash string) error {
	return s.tx.QueryRow(ctx, `SELECT identity_id FROM local_credentials WHERE identity_id=$1 AND username=$2 AND password_hash=$3 FOR UPDATE`, id, username, hash).Scan(&id)
}
func (s authenticationStore) ManagementWorkspace(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM local_auth_bootstrap WHERE singleton`).Scan(&id)
	return id, err
}
func (s authenticationStore) SetPassword(ctx context.Context, id uuid.UUID, hash string, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE local_credentials SET password_hash=$2,password_changed_at=$3,updated_at=$3 WHERE identity_id=$1`, id, hash, now)
	return err
}
func (s authenticationStore) RevokeIdentitySessions(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=$2 WHERE identity_id=$1 AND revoked_at IS NULL`, id, now)
	return err
}
func (t *transaction) AuthenticationStore() authstore.Store { return authenticationStore{t} }
func (b *Backend) ReadLocalCredential(ctx context.Context, username string) (authstore.Credential, error) {
	var c authstore.Credential
	err := b.QueryRow(ctx, `SELECT i.id,c.password_hash,i.disabled_at IS NOT NULL FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE c.username=$1 AND i.issuer='local' AND i.subject=c.username`, username).Scan(&c.ID, &c.Hash, &c.Disabled)
	return c, err
}
func (s authenticationStore) InsertCredential(ctx context.Context, id uuid.UUID, username, hash string, now time.Time) error {
	if _, err := s.tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'local',$2,$3,$3)`, id, username, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO local_credentials(identity_id,username,password_hash,created_at,updated_at,password_changed_at) VALUES($1,$2,$3,$4,$4,$4)`, id, username, hash, now)
	return err
}
func (s authenticationStore) LockCredential(ctx context.Context, id uuid.UUID, issuer, subject, hash string) (uuid.UUID, error) {
	err := s.tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=$1 AND i.issuer=$2 AND i.subject=$3 AND c.username=i.subject AND c.password_hash=$4 AND i.disabled_at IS NULL FOR UPDATE OF i,c`, id, issuer, subject, hash).Scan(&id)
	return id, err
}
func (s authenticationStore) InsertSession(ctx context.Context, id, identity uuid.UUID, expires time.Time, glass bool, now time.Time) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,break_glass,created_at) VALUES($1,$2,$3,$4,$5)`, id, identity, expires, glass, now)
	return err
}
func (s authenticationStore) Session(ctx context.Context, id, identity uuid.UUID) (authstore.Session, error) {
	var session authstore.Session
	err := s.tx.QueryRow(ctx, `SELECT i.issuer,i.subject,s.break_glass,s.expires_at FROM auth_sessions s JOIN identities i ON i.id=s.identity_id WHERE s.id=$1 AND s.identity_id=$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND i.disabled_at IS NULL`, id, identity).Scan(&session.Issuer, &session.Subject, &session.BreakGlass, &session.ExpiresAt)
	return session, err
}
func (s authenticationStore) RevokeSession(ctx context.Context, id, identity uuid.UUID) error {
	_, err := s.tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now() WHERE id=$1 AND identity_id=$2 AND revoked_at IS NULL`, id, identity)
	return err
}
func (s authenticationStore) ReserveAttempt(ctx context.Context, username string, lease uuid.UUID, capacity int, window, duration time.Duration) (bool, error) {
	if _, err := s.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734821033)`); err != nil {
		return false, err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE expires_at<=statement_timestamp()`); err != nil {
		return false, err
	}
	var exists bool
	if err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_attempts WHERE username=$1)`, username).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		var count int
		if err := s.tx.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&count); err != nil {
			return false, err
		}
		if count >= capacity {
			return false, authstore.ErrAttemptCapacity
		}
		if _, err := s.tx.Exec(ctx, `INSERT INTO local_auth_attempts(username,window_until,expires_at) VALUES($1,clock_timestamp()+$2::interval,clock_timestamp()+$2::interval)`, username, window.String()); err != nil {
			return false, err
		}
	}
	n, err := s.tx.Exec(ctx, `UPDATE local_auth_attempts SET failures=CASE WHEN window_until<=clock_timestamp() THEN 0 ELSE failures END,window_until=CASE WHEN window_until<=clock_timestamp() THEN clock_timestamp()+$3::interval ELSE window_until END,lease_id=$2,lease_until=clock_timestamp()+$4::interval,expires_at=GREATEST(CASE WHEN window_until<=clock_timestamp() THEN clock_timestamp()+$3::interval ELSE window_until END,clock_timestamp()+$4::interval) WHERE username=$1 AND blocked_until<=clock_timestamp() AND lease_until<=clock_timestamp()`, username, lease, window.String(), duration.String())
	return n == 1, err
}
func (s authenticationStore) FinishAttempt(ctx context.Context, username string, lease uuid.UUID, failed bool) error {
	_, err := s.tx.Exec(ctx, `UPDATE local_auth_attempts SET failures=CASE WHEN $3 THEN LEAST(failures+1,14) ELSE failures END,blocked_until=CASE WHEN $3 AND failures>=4 THEN clock_timestamp()+LEAST(300,power(2,LEAST(failures-4,9))) * interval '1 second' ELSE blocked_until END,expires_at=GREATEST(window_until,CASE WHEN $3 AND failures>=4 THEN clock_timestamp()+LEAST(300,power(2,LEAST(failures-4,9))) * interval '1 second' ELSE blocked_until END),lease_id=NULL,lease_until='-infinity' WHERE username=$1 AND lease_id=$2 AND lease_until>clock_timestamp()`, username, lease, failed)
	return err
}
func (s authenticationStore) ClearAttempt(ctx context.Context, username string, lease uuid.UUID) (bool, error) {
	n, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=$1 AND lease_id=$2 AND lease_until>clock_timestamp()`, username, lease)
	return n == 1, err
}
