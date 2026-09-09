package mysql

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/google/uuid"
	"time"
)

type rbacStore struct{ database.Store }

func (b *Backend) RBACStore() rbacstore.Store     { return rbacStore{b} }
func (t *transaction) RBACStore() rbacstore.Store { return rbacStore{t} }
func (s rbacStore) LockManagement(ctx context.Context) error {
	tx, ok := s.Store.(database.Tx)
	if !ok {
		return database.ErrUnsupported
	}
	return LockTransaction(ctx, tx, "pg-advisory:734821032")
}
func (s rbacStore) Roles(ctx context.Context, identity, workspace uuid.UUID, resourceType string, resource uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT role_name FROM role_bindings WHERE identity_id=? AND workspace_id=? AND (resource_type='workspace' OR (CAST(resource_type AS BINARY)=CAST(? AS BINARY) AND resource_id=?))`, UUIDBytes(identity), UUIDBytes(workspace), resourceType, rbacID(resource))
}
func (s rbacStore) WorkspaceRoles(ctx context.Context, identity, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT role_name FROM role_bindings WHERE identity_id=? AND workspace_id=? AND resource_type='workspace'`, UUIDBytes(identity), UUIDBytes(workspace))
}
func (s rbacStore) Node(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=?`, UUIDBytes(id))
}
func (s rbacStore) Operation(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT workspace_id,node_id FROM operations WHERE id=?`, UUIDBytes(id))
}
func (s rbacStore) Workspace(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=?)`, UUIDBytes(id))
}
func (s rbacStore) AuthorizedWorkspaces(ctx context.Context, id uuid.UUID, breakGlass bool) (database.Rows, error) {
	if breakGlass {
		return s.Query(ctx, `SELECT id,'PlatformAdmin' FROM workspaces`)
	}
	return s.Query(ctx, `SELECT DISTINCT workspace_id,role_name FROM role_bindings WHERE identity_id=?`, UUIDBytes(id))
}
func (s rbacStore) Insert(ctx context.Context, b rbacstore.Binding) error {
	at, err := value.FromTime(b.At)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at,approval_id) VALUES(?,?,?,?,?,?,?,?,?)`, UUIDBytes(b.ID), UUIDBytes(b.IdentityID), UUIDBytes(b.WorkspaceID), b.Role, b.ResourceType, rbacID(b.ResourceID), UUIDBytes(b.ActorID), at, rbacID(b.ApprovalID))
	return err
}
func (s rbacStore) CompleteBootstrap(ctx context.Context, workspace, identity uuid.UUID, at time.Time) error {
	stamp, err := value.FromTime(at)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE local_auth_bootstrap SET completion_pending=false,completed_at=? WHERE completion_pending AND workspace_id=? AND identity_id<>?`, stamp, UUIDBytes(workspace), UUIDBytes(identity))
	return err
}
func rbacID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return UUIDBytes(id)
}
