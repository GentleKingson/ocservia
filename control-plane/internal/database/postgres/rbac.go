package postgres

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/google/uuid"
	"time"
)

type rbacStore struct{ database.Store }

func (b *Backend) RBACStore() rbacstore.Store     { return rbacStore{b} }
func (t *transaction) RBACStore() rbacstore.Store { return rbacStore{t} }
func (s rbacStore) LockManagement(ctx context.Context) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(734821032)`)
	return err
}
func (s rbacStore) Roles(ctx context.Context, identity, workspace uuid.UUID, resourceType string, resource uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT role_name FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2 AND (resource_type='workspace' OR (resource_type=$3 AND resource_id=$4))`, identity, workspace, resourceType, rbacID(resource))
}
func (s rbacStore) WorkspaceRoles(ctx context.Context, identity, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT role_name FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2 AND resource_type='workspace'`, identity, workspace)
}
func (s rbacStore) Node(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1`, id)
}
func (s rbacStore) Operation(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT workspace_id,node_id FROM operations WHERE id=$1`, id)
}
func (s rbacStore) Workspace(ctx context.Context, id uuid.UUID) database.Row {
	return s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, id)
}
func (s rbacStore) AuthorizedWorkspaces(ctx context.Context, id uuid.UUID, breakGlass bool) (database.Rows, error) {
	if breakGlass {
		return s.Query(ctx, `SELECT id,'PlatformAdmin' FROM workspaces`)
	}
	return s.Query(ctx, `SELECT DISTINCT workspace_id,role_name FROM role_bindings WHERE identity_id=$1`, id)
}
func (s rbacStore) Insert(ctx context.Context, b rbacstore.Binding) error {
	_, err := s.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at,approval_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, b.ID, b.IdentityID, b.WorkspaceID, b.Role, b.ResourceType, rbacID(b.ResourceID), b.ActorID, b.At, rbacID(b.ApprovalID))
	return err
}
func (s rbacStore) CompleteBootstrap(ctx context.Context, workspace, identity uuid.UUID, at time.Time) error {
	_, err := s.Exec(ctx, `UPDATE local_auth_bootstrap SET completion_pending=false,completed_at=$1 WHERE completion_pending AND workspace_id=$2 AND identity_id<>$3`, at, workspace, identity)
	return err
}
func rbacID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
