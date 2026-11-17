package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type mutationTx struct {
	database.Tx
	authentication authstore.Store
}

func (t mutationTx) AuthenticationStore() authstore.Store { return t.authentication }

type mutationAuthentication struct {
	authstore.Store
	sessionErr        error
	identity, session uuid.UUID
	workspace         uuid.UUID
	breakGlass        bool
	queried           bool
}

func (a *mutationAuthentication) LockActiveSession(_ context.Context, identity, session uuid.UUID, local bool) error {
	if local || identity != a.identity || session != a.session {
		return database.ErrNotFound
	}
	return a.sessionErr
}

func (a *mutationAuthentication) IsWorkspaceAdminOrBreakGlass(_ context.Context, workspace, identity, session uuid.UUID) (bool, error) {
	a.queried = true
	if session == a.session && a.breakGlass {
		return true, nil
	}
	return identity == a.identity && workspace == a.workspace, nil
}

func TestSingleAdminMutationAuthorization(t *testing.T) {
	identity, session, workspace := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, tc := range []struct {
		name           string
		sessionErr     error
		crossWorkspace bool
		breakGlass     bool
		allowed        bool
	}{
		{name: "workspace-admin", allowed: true},
		{name: "break-glass", crossWorkspace: true, breakGlass: true, allowed: true},
		{name: "disabled-or-revoked-session", sessionErr: database.ErrNotFound},
		{name: "different-workspace", crossWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authStore := &mutationAuthentication{identity: identity, session: session, workspace: workspace, sessionErr: tc.sessionErr, breakGlass: tc.breakGlass}
			target := workspace
			if tc.crossWorkspace {
				target = uuid.Must(uuid.NewV7())
			}
			err := AuthorizeMutationTx(t.Context(), mutationTx{authentication: authStore}, identity, session, target)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected denial, got %v", err)
			}
			if tc.sessionErr != nil && authStore.queried {
				t.Fatal("queried workspace authority after invalid session")
			}
		})
	}
}
