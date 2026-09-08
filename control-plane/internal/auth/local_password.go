package auth

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ChangeLocalPassword has no target identity: only the authenticated Local
// principal may change its own credential. Success revokes every old session.
func (s *Service) ChangeLocalPassword(ctx context.Context, actor Principal, currentPassword, newPassword, requestID string) (resultErr error) {
	if !s.localEnabled {
		return ErrLocalDisabled
	}
	if actor.Issuer != LocalIssuer || actor.BreakGlass || actor.IdentityID == uuid.Nil || actor.SessionID == uuid.Nil || requestID == "" {
		return ErrUnauthenticated
	}
	if len(currentPassword) == 0 || len(currentPassword) > maxPasswordBytes {
		return ErrUnauthenticated
	}
	if err := ValidateNewPassword(newPassword); err != nil {
		return err
	}
	var username string
	if err := s.pool.QueryRow(ctx, `SELECT c.username FROM local_credentials c JOIN identities i ON i.id=c.identity_id WHERE i.id=$1 AND i.issuer='local' AND i.subject=c.username AND i.disabled_at IS NULL`, actor.IdentityID).Scan(&username); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		return err
	}
	lease, err := s.reserveLocalAttempt(ctx, username)
	if err != nil {
		return err
	}
	failed, committed := false, false
	defer func() {
		if !committed {
			if err := s.finishLocalAttempt(username, lease, failed); err != nil {
				resultErr = err
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, localAttemptLease)
	defer cancel()
	credential, err := s.localCredential(ctx, username)
	if err != nil {
		return err
	}
	valid, err := verifyPassword(credential.passwordHash, currentPassword)
	if err != nil {
		return err
	}
	if !valid {
		failed = true
		return ErrUnauthenticated
	}
	if credential.disabled || credential.identityID != actor.IdentityID {
		return ErrUnauthenticated
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockActiveSession(ctx, tx, actor.IdentityID, actor.SessionID, true); err != nil {
		return err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT identity_id FROM local_credentials WHERE identity_id=$1 AND username=$2 AND password_hash=$3 FOR UPDATE`, actor.IdentityID, username, credential.passwordHash).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		return err
	}
	if err := clearLocalAttempt(ctx, tx, username, lease); err != nil {
		return err
	}
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM local_auth_bootstrap WHERE singleton`).Scan(&workspaceID); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE local_credentials SET password_hash=$2,password_changed_at=$3,updated_at=$3 WHERE identity_id=$1`, id, hash, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=$2 WHERE identity_id=$1 AND revoked_at IS NULL`, id, now); err != nil {
		return err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: id.String(), SessionID: &actor.SessionID, Action: "local_user.change-password", ResourceType: "local_user", ResourceID: id, RequestID: requestID, Result: "succeeded", At: now}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
