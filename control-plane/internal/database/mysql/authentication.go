package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type authenticationStore struct{ tx database.Tx }

func (b *Backend) HasLocalCredential(ctx context.Context, id uuid.UUID) (bool, error) {
	var found bool
	err := b.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=? AND CAST(i.issuer AS BINARY)=_binary'local' AND CAST(i.subject AS BINARY)=CAST(c.username AS BINARY))`, UUIDBytes(id)).Scan(&found)
	return found, err
}

func (s authenticationStore) LockManagement(ctx context.Context) error {
	if err := LockTransaction(ctx, s.tx, "pg-advisory:734821032"); err != nil {
		return err
	}
	// Match OIDC's exact-key-before-identity order before locking either the
	// acting or target identity. Identity UPDATE triggers take this same guard.
	return LockExactKey(ctx, s.tx, "identities")
}
func (s authenticationStore) WorkspaceExists(ctx context.Context, id uuid.UUID) error {
	var raw []byte
	return s.tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=? LOCK IN SHARE MODE`, UUIDBytes(id)).Scan(&raw)
}
func (s authenticationStore) Bootstrap(ctx context.Context) (authstore.Bootstrap, error) {
	var b authstore.Bootstrap
	var identity, workspace []byte
	err := s.tx.QueryRow(ctx, `SELECT identity_id,workspace_id,completion_pending FROM local_auth_bootstrap WHERE singleton=1 FOR UPDATE`).Scan(&identity, &workspace, &b.Pending)
	if err != nil {
		return b, err
	}
	b.IdentityID, err = uuid.FromBytes(identity)
	if err != nil {
		return b, err
	}
	b.WorkspaceID, err = uuid.FromBytes(workspace)
	return b, err
}
func (s authenticationStore) ValidatePendingCredential(ctx context.Context, id, workspace uuid.UUID, hash string) error {
	if err := s.LockLocalIdentity(ctx, id); err != nil {
		return err
	}
	var raw []byte
	return s.tx.QueryRow(ctx, `SELECT i.id FROM identities i JOIN local_credentials c ON c.identity_id=i.id WHERE i.id=? AND i.disabled_at IS NULL AND CAST(c.password_hash AS BINARY)=? AND EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=i.id AND workspace_id=? AND resource_type='workspace' AND role_name='PlatformAdmin') AND NOT EXISTS(SELECT 1 FROM role_bindings WHERE workspace_id=? AND identity_id<>i.id AND role_name IN ('SecurityAdmin','PlatformAdmin'))`, UUIDBytes(id), []byte(hash), UUIDBytes(workspace), UUIDBytes(workspace)).Scan(&raw)
}
func (s authenticationStore) Initialized(ctx context.Context) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_bootstrap) OR EXISTS(SELECT 1 FROM identities i JOIN role_bindings b ON b.identity_id=i.id WHERE CAST(i.issuer AS BINARY)=_binary'local' AND b.role_name IN ('SecurityAdmin','PlatformAdmin'))`).Scan(&yes)
	return yes, err
}
func (s authenticationStore) BindRole(ctx context.Context, id, identity, workspace uuid.UUID, role string, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,?,'workspace',?)`, UUIDBytes(id), UUIDBytes(identity), UUIDBytes(workspace), role, stamp)
	return err
}
func (s authenticationStore) InsertBootstrap(ctx context.Context, id, workspace uuid.UUID, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO local_auth_bootstrap(singleton,identity_id,workspace_id,created_at,completed_at) VALUES(true,?,?,?,?)`, UUIDBytes(id), UUIDBytes(workspace), stamp, stamp)
	return err
}
func (s authenticationStore) CompleteBootstrap(ctx context.Context, approver uuid.UUID, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE local_auth_bootstrap SET completion_pending=false,completed_at=?,approver_identity_id=? WHERE singleton=1`, stamp, UUIDBytes(approver))
	return err
}
func (s authenticationStore) MayManage(ctx context.Context, actor, workspace, session uuid.UUID) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_bindings WHERE identity_id=? AND workspace_id=? AND role_name='PlatformAdmin' AND resource_type='workspace') OR EXISTS(SELECT 1 FROM auth_sessions WHERE id=? AND break_glass)`, UUIDBytes(actor), UUIDBytes(workspace), UUIDBytes(session)).Scan(&yes)
	return yes, err
}
func (s authenticationStore) LockLocalIdentity(ctx context.Context, id uuid.UUID) error {
	var subject string
	if err := s.tx.QueryRow(ctx, `SELECT subject FROM identities WHERE id=? AND CAST(issuer AS BINARY)=_binary'local' FOR UPDATE`, UUIDBytes(id)).Scan(&subject); err != nil {
		return err
	}
	var raw []byte
	return s.tx.QueryRow(ctx, `SELECT identity_id FROM local_credentials WHERE identity_id=? AND CAST(username AS BINARY)=? FOR UPDATE`, UUIDBytes(id), []byte(subject)).Scan(&raw)
}
func (s authenticationStore) Protected(ctx context.Context, workspace, id uuid.UUID) (bool, error) {
	var yes bool
	err := s.tx.QueryRow(ctx, `WITH effective AS (SELECT DISTINCT i.id,b.role_name FROM identities i JOIN local_credentials c ON c.identity_id=i.id JOIN role_bindings b ON b.identity_id=i.id WHERE i.disabled_at IS NULL AND CAST(i.issuer AS BINARY)=_binary'local' AND CAST(i.subject AS BINARY)=CAST(c.username AS BINARY) AND b.workspace_id=? AND b.resource_type='workspace' AND b.role_name IN ('PlatformAdmin','SecurityAdmin')) SELECT EXISTS(SELECT 1 FROM effective WHERE id=?) AND (NOT EXISTS(SELECT 1 FROM effective WHERE id<>? AND role_name='PlatformAdmin') OR (SELECT count(DISTINCT id) FROM effective WHERE id<>?)<2)`, UUIDBytes(workspace), UUIDBytes(id), UUIDBytes(id), UUIDBytes(id)).Scan(&yes)
	return yes, err
}
func (s authenticationStore) Disable(ctx context.Context, id uuid.UUID, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE identities SET disabled_at=COALESCE(disabled_at,?),updated_at=? WHERE id=?`, stamp, stamp, UUIDBytes(id))
	return err
}
func (s authenticationStore) DeleteIdentityAttempts(ctx context.Context, id uuid.UUID) error {
	_, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=(SELECT username FROM local_credentials WHERE identity_id=?)`, UUIDBytes(id))
	return err
}
func (s authenticationStore) BreakGlassUsed(ctx context.Context, fingerprint []byte) (bool, error) {
	var used bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM break_glass_uses WHERE credential_fingerprint=? AND rotation_required=true)`, fingerprint).Scan(&used)
	return used, err
}
func (s authenticationStore) BreakGlassIdentity(ctx context.Context, id uuid.UUID, now time.Time) (uuid.UUID, error) {
	stamp, err := value.FromTime(now)
	if err != nil {
		return uuid.Nil, err
	}
	if err := LockExactKey(ctx, s.tx, "identities"); err != nil {
		return uuid.Nil, err
	}
	var raw []byte
	err = s.tx.QueryRow(ctx, `SELECT id FROM identities WHERE CAST(issuer AS BINARY)=_binary'break-glass' AND CAST(subject AS BINARY)=_binary'offline' FOR UPDATE`).Scan(&raw)
	if errors.Is(err, database.ErrNotFound) {
		_, err = s.tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,display_name,created_at,updated_at) VALUES(?,'break-glass','offline','Break-glass',?,?)`, UUIDBytes(id), stamp, stamp)
		return id, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	id, err = uuid.FromBytes(raw)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = s.tx.Exec(ctx, `UPDATE identities SET updated_at=? WHERE id=?`, stamp, raw)
	return id, err
}
func (s authenticationStore) RecordBreakGlass(ctx context.Context, fingerprint []byte, id, session, alert uuid.UUID, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `INSERT INTO break_glass_uses(credential_fingerprint,identity_id,used_at,source_session_id,rotation_required) VALUES(?,?,?,?,true)`, fingerprint, UUIDBytes(id), stamp, UUIDBytes(session)); err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO security_alerts(id,severity,kind,source_session_id,created_at) VALUES(?,'critical','break_glass.used',?,?)`, UUIDBytes(alert), UUIDBytes(session), stamp)
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
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := uuid.FromBytes(raw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s authenticationStore) ActiveLocalUsername(ctx context.Context, id uuid.UUID) (string, error) {
	var username string
	err := s.tx.QueryRow(ctx, `SELECT c.username FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE i.id=? AND CAST(i.issuer AS BINARY)=_binary'local' AND CAST(i.subject AS BINARY)=CAST(c.username AS BINARY) AND i.disabled_at IS NULL`, UUIDBytes(id)).Scan(&username)
	return username, err
}
func (s authenticationStore) LockActiveSession(ctx context.Context, identity, session uuid.UUID, local bool) error {
	var id []byte
	if err := s.tx.QueryRow(ctx, `SELECT id FROM identities WHERE id=? AND disabled_at IS NULL AND (NOT ? OR CAST(issuer AS BINARY)=_binary'local') FOR UPDATE`, UUIDBytes(identity), local).Scan(&id); err != nil {
		return err
	}
	var expires value.Timestamp
	if err := s.tx.QueryRow(ctx, `SELECT expires_at FROM auth_sessions WHERE id=? AND identity_id=? AND revoked_at IS NULL AND (NOT ? OR NOT break_glass) FOR UPDATE`, UUIDBytes(session), UUIDBytes(identity), local).Scan(&expires); err != nil {
		return err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return err
	}
	if !expires.Valid || expires.Micros <= now {
		return database.ErrNotFound
	}
	return nil
}
func (s authenticationStore) LockPassword(ctx context.Context, id uuid.UUID, username, hash string) error {
	var raw []byte
	return s.tx.QueryRow(ctx, `SELECT identity_id FROM local_credentials WHERE identity_id=? AND CAST(username AS BINARY)=? AND CAST(password_hash AS BINARY)=? FOR UPDATE`, UUIDBytes(id), []byte(username), []byte(hash)).Scan(&raw)
}
func (s authenticationStore) ManagementWorkspace(ctx context.Context) (uuid.UUID, error) {
	var raw []byte
	err := s.tx.QueryRow(ctx, `SELECT workspace_id FROM local_auth_bootstrap WHERE singleton=1`).Scan(&raw)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.FromBytes(raw)
}
func (s authenticationStore) SetPassword(ctx context.Context, id uuid.UUID, hash string, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE local_credentials SET password_hash=?,password_changed_at=?,updated_at=? WHERE identity_id=?`, hash, stamp, stamp, UUIDBytes(id))
	return err
}
func (s authenticationStore) RevokeIdentitySessions(ctx context.Context, id uuid.UUID, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=? WHERE identity_id=? AND revoked_at IS NULL`, stamp, UUIDBytes(id))
	return err
}
func (t *transaction) AuthenticationStore() authstore.Store { return authenticationStore{t} }
func (b *Backend) ReadLocalCredential(ctx context.Context, username string) (authstore.Credential, error) {
	var c authstore.Credential
	var raw []byte
	err := b.QueryRow(ctx, `SELECT i.id,c.password_hash,i.disabled_at IS NOT NULL FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE c.username=? AND CAST(i.issuer AS BINARY)=_binary'local' AND CAST(i.subject AS BINARY)=CAST(c.username AS BINARY)`, username).Scan(&raw, &c.Hash, &c.Disabled)
	if err == nil {
		c.ID, err = uuid.FromBytes(raw)
	}
	return c, err
}
func (s authenticationStore) InsertCredential(ctx context.Context, id uuid.UUID, username, hash string, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	if err := LockExactKey(ctx, s.tx, "identities"); err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,'local',?,?,?)`, UUIDBytes(id), username, stamp, stamp); err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO local_credentials(identity_id,username,password_hash,created_at,updated_at,password_changed_at) VALUES(?,?,?,?,?,?)`, UUIDBytes(id), username, hash, stamp, stamp, stamp)
	return err
}
func (s authenticationStore) LockCredential(ctx context.Context, id uuid.UUID, issuer, subject, hash string) (uuid.UUID, error) {
	var raw []byte
	// Lock the identity before its credential, matching mutation/session ordering.
	if err := s.tx.QueryRow(ctx, `SELECT id FROM identities WHERE id=? AND CAST(issuer AS BINARY)=? AND CAST(subject AS BINARY)=? AND disabled_at IS NULL FOR UPDATE`, UUIDBytes(id), []byte(issuer), []byte(subject)).Scan(&raw); err != nil {
		return uuid.Nil, err
	}
	err := s.tx.QueryRow(ctx, `SELECT identity_id FROM local_credentials WHERE identity_id=? AND CAST(username AS BINARY)=? AND CAST(password_hash AS BINARY)=? FOR UPDATE`, UUIDBytes(id), []byte(subject), []byte(hash)).Scan(&raw)
	return id, err
}
func (s authenticationStore) InsertSession(ctx context.Context, id, identity uuid.UUID, expires time.Time, glass bool, now time.Time) error {
	stamp, err := value.FromTime(now)
	if err != nil {
		return err
	}
	end, err := value.FromTime(expires)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO auth_sessions(id,identity_id,expires_at,break_glass,created_at) VALUES(?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(identity), end, glass, stamp)
	return err
}
func (s authenticationStore) Session(ctx context.Context, id, identity uuid.UUID) (authstore.Session, error) {
	var session authstore.Session
	err := s.tx.QueryRow(ctx, `SELECT i.issuer,i.subject,s.break_glass,s.expires_at FROM auth_sessions s JOIN identities i ON i.id=s.identity_id WHERE s.id=? AND s.identity_id=? AND s.revoked_at IS NULL AND s.expires_at>TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6)) AND i.disabled_at IS NULL`, UUIDBytes(id), UUIDBytes(identity)).Scan(&session.Issuer, &session.Subject, &session.BreakGlass, &session.ExpiresAt)
	return session, err
}
func (s authenticationStore) RevokeSession(ctx context.Context, id, identity uuid.UUID) error {
	_, err := s.tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6)) WHERE id=? AND identity_id=? AND revoked_at IS NULL`, UUIDBytes(id), UUIDBytes(identity))
	return err
}
func (s authenticationStore) clock(ctx context.Context) (int64, error) {
	var now int64
	err := s.tx.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6))`).Scan(&now)
	return now, err
}
func (s authenticationStore) ReserveAttempt(ctx context.Context, username string, lease uuid.UUID, capacity int, window, duration time.Duration) (bool, error) {
	if err := LockTransaction(ctx, s.tx, "pg-advisory:734821033"); err != nil {
		return false, err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return false, err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE expires_at<=?`, now); err != nil {
		return false, err
	}
	var failures int
	var windowUntil, blockedUntil, leaseUntil int64
	err = s.tx.QueryRow(ctx, `SELECT failures,window_until,blocked_until,lease_until FROM local_auth_attempts WHERE username=? FOR UPDATE`, username).Scan(&failures, &windowUntil, &blockedUntil, &leaseUntil)
	if errors.Is(err, database.ErrNotFound) {
		var count int
		if err := s.tx.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts`).Scan(&count); err != nil {
			return false, err
		}
		if count >= capacity {
			return false, authstore.ErrAttemptCapacity
		}
		windowUntil = now + window.Microseconds()
		if _, err := s.tx.Exec(ctx, `INSERT INTO local_auth_attempts(username,window_until,expires_at) VALUES(?,?,?)`, username, windowUntil, windowUntil); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	// Read a fresh server clock after any row-lock wait, not statement start.
	now, err = s.clock(ctx)
	if err != nil {
		return false, err
	}
	if blockedUntil > now || leaseUntil > now {
		return false, nil
	}
	if windowUntil <= now {
		failures = 0
		windowUntil = now + window.Microseconds()
	}
	leaseUntil = now + duration.Microseconds()
	_, err = s.tx.Exec(ctx, `UPDATE local_auth_attempts SET failures=?,window_until=?,lease_id=?,lease_until=?,expires_at=? WHERE username=?`, failures, windowUntil, UUIDBytes(lease), leaseUntil, max(windowUntil, leaseUntil), username)
	return err == nil, err
}
func (s authenticationStore) FinishAttempt(ctx context.Context, username string, lease uuid.UUID, failed bool) error {
	var failures int
	var windowUntil, blockedUntil, leaseUntil int64
	err := s.tx.QueryRow(ctx, `SELECT failures,window_until,blocked_until,lease_until FROM local_auth_attempts WHERE username=? AND lease_id=? FOR UPDATE`, username, UUIDBytes(lease)).Scan(&failures, &windowUntil, &blockedUntil, &leaseUntil)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return err
	}
	if leaseUntil <= now {
		return nil
	}
	if failed {
		if failures >= 4 {
			blockedUntil = now + int64(min(300, 1<<min(failures-4, 9)))*1000000
		}
		failures = min(failures+1, 14)
	}
	_, err = s.tx.Exec(ctx, `UPDATE local_auth_attempts SET failures=?,blocked_until=?,expires_at=?,lease_id=NULL,lease_until=-9223372036854775808 WHERE username=? AND lease_id=?`, failures, blockedUntil, max(windowUntil, blockedUntil), username, UUIDBytes(lease))
	return err
}
func (s authenticationStore) ClearAttempt(ctx context.Context, username string, lease uuid.UUID) (bool, error) {
	var expires int64
	err := s.tx.QueryRow(ctx, `SELECT lease_until FROM local_auth_attempts WHERE username=? AND lease_id=? FOR UPDATE`, username, UUIDBytes(lease)).Scan(&expires)
	if errors.Is(err, database.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now, err := s.clock(ctx)
	if err != nil {
		return false, err
	}
	if expires <= now {
		return false, nil
	}
	n, err := s.tx.Exec(ctx, `DELETE FROM local_auth_attempts WHERE username=? AND lease_id=?`, username, UUIDBytes(lease))
	return n == 1, err
}
