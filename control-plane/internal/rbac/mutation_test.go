package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/authstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/google/uuid"
)

type mutationTx struct {
	database.Tx
	authentication authstore.Store
	roles          rbacstore.Store
}

func (t mutationTx) AuthenticationStore() authstore.Store { return t.authentication }
func (t mutationTx) RBACStore() rbacstore.Store           { return t.roles }

type mutationAuthentication struct {
	authstore.Store
	err               error
	identity, session uuid.UUID
}

func (a *mutationAuthentication) LockActiveSession(_ context.Context, identity, session uuid.UUID, local bool) error {
	if local || identity != a.identity || session != a.session {
		return database.ErrNotFound
	}
	return a.err
}

func (a *mutationAuthentication) MayManage(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}

type mutationRoles struct {
	rbacstore.Store
	role      string
	workspace uuid.UUID
	queried   bool
}

func (r *mutationRoles) Roles(_ context.Context, _ uuid.UUID, workspace uuid.UUID, _ string, _ uuid.UUID) (database.Rows, error) {
	r.queried = true
	if workspace != r.workspace {
		return &mutationRows{}, nil
	}
	return &mutationRows{role: r.role}, nil
}

type mutationRows struct {
	role string
	read bool
}

func (r *mutationRows) Next() bool { return r.role != "" && !r.read }
func (r *mutationRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.role
	r.read = true
	return nil
}
func (*mutationRows) Err() error { return nil }
func (*mutationRows) Close()     {}

func TestSingleAdminMutationAuthorization(t *testing.T) {
	identity, session, workspace := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, tc := range []struct {
		name, role     string
		sessionErr     error
		crossWorkspace bool
		allowed        bool
	}{
		{name: "admin", role: "PlatformAdmin", allowed: true},
		{name: "viewer", role: "Viewer"},
		{name: "disabled-or-revoked-session", role: "PlatformAdmin", sessionErr: database.ErrNotFound},
		{name: "different-workspace", role: "PlatformAdmin", crossWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &mutationAuthentication{identity: identity, session: session, err: tc.sessionErr}
			roles := &mutationRoles{role: tc.role, workspace: workspace}
			target := workspace
			if tc.crossWorkspace {
				target = uuid.Must(uuid.NewV7())
			}
			err := AuthorizeMutationTx(t.Context(), mutationTx{authentication: auth, roles: roles}, identity, session, "certificate.private_key.export", Resource{WorkspaceID: target, Type: "node", ID: uuid.Must(uuid.NewV7())})
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected denial, got %v", err)
			}
			if tc.sessionErr != nil && roles.queried {
				t.Fatal("queried authority after invalid session")
			}
		})
	}
}
