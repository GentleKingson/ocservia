package auth

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
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
	if err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		username, err = store.ActiveLocalUsername(ctx, actor.IdentityID)
		return err
	}); err != nil {
		if errors.Is(err, database.ErrNotFound) {
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
	err = s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		if err := store.LockActiveSession(ctx, actor.IdentityID, actor.SessionID, true); err != nil {
			return ErrUnauthenticated
		}
		id := actor.IdentityID
		if err := store.LockPassword(ctx, id, username, credential.passwordHash); err != nil {
			if errors.Is(err, database.ErrNotFound) {
				return ErrUnauthenticated
			}
			return err
		}
		cleared, err := store.ClearAttempt(ctx, username, lease)
		if err != nil {
			return err
		}
		if !cleared {
			return ErrUnauthenticated
		}
		workspaceID, err := store.ManagementWorkspace(ctx)
		if err != nil {
			return err
		}
		now := s.now()
		if err := store.SetPassword(ctx, id, hash, now); err != nil {
			return err
		}
		if err := store.RevokeIdentitySessions(ctx, id, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: id.String(), SessionID: &actor.SessionID, Action: "local_user.change-password", ResourceType: "local_user", ResourceID: id, RequestID: requestID, Result: "succeeded", At: now})
	})
	if err != nil {
		return err
	}
	committed = true
	return nil
}
