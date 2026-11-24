package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var (
	ErrLocalInitialized      = errors.New("Local authentication is already initialized")
	ErrLocalInvalid          = errors.New("invalid Local user request")
	ErrLocalWorkspaceMissing = errors.New("management workspace does not exist; provision it through the protected administrative database connection before initialization")
	ErrLocalAdminUnavailable = errors.New("no active bootstrapped Local administrator with a management workspace binding")
)

// BootstrapLocalAdmin is an explicit, atomic one-shot for creating the single Local administrator.
func (s *Service) BootstrapLocalAdmin(ctx context.Context, username, password string, workspaceID uuid.UUID) (uuid.UUID, error) {
	if !s.localEnabled {
		return uuid.Nil, ErrLocalDisabled
	}
	username, err := normalizeLocalUsername(username)
	if err != nil || workspaceID == uuid.Nil {
		return uuid.Nil, ErrLocalInvalid
	}
	hash, err := hashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	err = s.withAuthentication(ctx, func(tx database.Tx, store authstore.Store) error {
		if err := store.LockManagement(ctx); err != nil {
			return err
		}
		if err := store.WorkspaceExists(ctx, workspaceID); err != nil {
			if errors.Is(err, database.ErrNotFound) {
				return ErrLocalWorkspaceMissing
			}
			return err
		}
		initialized, err := store.Initialized(ctx)
		if err != nil {
			return err
		}
		if initialized {
			return ErrLocalInitialized
		}
		now := s.now()
		if err := store.InsertCredential(ctx, id, username, hash, now); err != nil {
			return err
		}
		if err := store.BindWorkspaceAdmin(ctx, workspaceID, id, now); err != nil {
			return err
		}
		if err := store.InsertBootstrap(ctx, id, workspaceID, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "controller", ActorID: "local-bootstrap", Action: "local_user.bootstrap", ResourceType: "local_user", ResourceID: id, RequestID: uuid.NewString(), Result: "succeeded", Reason: "initial Local administrator", At: now})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ResetLocalAdminPassword generates a credential for the original administrator.
// Only the trusted, interactive Controller command calls this method.
func (s *Service) ResetLocalAdminPassword(ctx context.Context) (string, string, error) {
	if !s.localEnabled {
		return "", "", ErrLocalDisabled
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", fmt.Errorf("generate administrator password: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(random[:])
	hash, err := hashPassword(password)
	if err != nil {
		return "", "", err
	}

	var username string
	err = database.WithinRetry(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := authstore.From(tx)
		if err != nil {
			return err
		}
		if err := store.LockManagement(ctx); err != nil {
			return err
		}
		id, workspace, account, err := store.LockBootstrapAdmin(ctx)
		if errors.Is(err, database.ErrNotFound) {
			return ErrLocalAdminUnavailable
		}
		if err != nil {
			return err
		}
		username = account
		now := s.now()
		if err := store.SetPassword(ctx, id, hash, now); err != nil {
			return err
		}
		if err := store.DeleteIdentityAttempts(ctx, id); err != nil {
			return err
		}
		if err := store.RevokeIdentitySessions(ctx, id, now); err != nil {
			return err
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{
			WorkspaceID: workspace, ActorType: "controller", ActorID: "local-admin-recovery",
			Action: "local_user.reset-password", ResourceType: "local_user", ResourceID: id,
			RequestID: uuid.NewString(), Result: "succeeded", Reason: "operator credential recovery", At: now,
		})
	})
	if err != nil {
		return "", "", err
	}
	return username, password, nil
}
