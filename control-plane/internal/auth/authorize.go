package auth

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

// AuthorizeWorkspace verifies that the principal is an administrator for workspaceID (or holds a break-glass session).
func (s *Service) AuthorizeWorkspace(ctx context.Context, principal Principal, workspaceID uuid.UUID) error {
	if principal.IdentityID == uuid.Nil || workspaceID == uuid.Nil {
		return ErrForbidden
	}
	var allowed bool
	if err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		if principal.BreakGlass {
			if err := store.WorkspaceExists(ctx, workspaceID); err != nil {
				return err
			}
			allowed = true
			return nil
		}
		var err error
		allowed, err = store.IsWorkspaceAdmin(ctx, workspaceID, principal.IdentityID)
		return err
	}); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// AuthorizedWorkspaces returns all workspaces visible to the principal.
func (s *Service) AuthorizedWorkspaces(ctx context.Context, principal Principal) ([]authstore.Workspace, error) {
	if principal.IdentityID == uuid.Nil {
		return nil, ErrForbidden
	}
	var out []authstore.Workspace
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		out, err = store.AuthorizedWorkspaces(ctx, principal.IdentityID, principal.BreakGlass)
		return err
	})
	return out, err
}

func (s *Service) WorkspaceExists(ctx context.Context, workspaceID uuid.UUID) error {
	return s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		return store.WorkspaceExists(ctx, workspaceID)
	})
}

func (s *Service) NodeWorkspace(ctx context.Context, nodeID uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		workspaceID, err = store.NodeWorkspace(ctx, nodeID)
		return err
	})
	return workspaceID, err
}

func (s *Service) OperationWorkspace(ctx context.Context, operationID uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		var err error
		workspaceID, err = store.OperationWorkspace(ctx, operationID)
		return err
	})
	return workspaceID, err
}

func (s *Service) BindWorkspaceAdmin(ctx context.Context, workspaceID, identityID uuid.UUID) error {
	return s.withAuthentication(ctx, func(_ database.Tx, store authstore.Store) error {
		return store.BindWorkspaceAdmin(ctx, workspaceID, identityID, s.now())
	})
}

// AuthorizeMutationTx locks the initiating active session and enforces single-admin or break-glass authority inside tx.
func AuthorizeMutationTx(ctx context.Context, tx database.Tx, identityID, sessionID, workspaceID uuid.UUID) error {
	if identityID == uuid.Nil || sessionID == uuid.Nil || workspaceID == uuid.Nil {
		return ErrForbidden
	}
	store := authstore.From(tx)
	if err := store.LockActiveSession(ctx, identityID, sessionID, false); err != nil {
		return ErrForbidden
	}
	allowed, err := store.IsWorkspaceAdminOrBreakGlass(ctx, workspaceID, identityID, sessionID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}
