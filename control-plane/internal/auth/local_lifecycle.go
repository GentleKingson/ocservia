package auth

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var (
	ErrLocalInitialized      = errors.New("Local authentication is already initialized")
	ErrLocalInvalid          = errors.New("invalid Local user request")
	ErrLocalWorkspaceMissing = errors.New("management workspace does not exist; provision it through the protected administrative database connection before initialization")
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
