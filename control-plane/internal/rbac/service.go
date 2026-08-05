package rbac

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrForbidden = errors.New("authorization denied")

type Resource struct {
	WorkspaceID uuid.UUID
	Type        string
	ID          uuid.UUID
}

type Service struct{ pool *pgxpool.Pool }

type BindingRequest struct {
	IdentityID, WorkspaceID, ResourceID, ActorID, SessionID uuid.UUID
	Role, ResourceType, RequestID, Reason                   string
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

var roleActions = map[string][]string{
	"Viewer":        {"node.read", "operation.read"},
	"Operator":      {"node.read", "operation.read", "session.disconnect", "session.terminate", "ip_ban.remove", "service.reload", "approval.request"},
	"UserManager":   {"node.read", "operation.read", "user.manage", "group.manage", "approval.request"},
	"ConfigManager": {"node.read", "operation.read", "config.plan", "config.apply", "approval.request"},
	"Auditor":       {"node.read", "operation.read", "audit.read", "audit.verify"},
	"SecurityAdmin": {"node.read", "operation.read", "enrollment_token.create", "node.approve", "node.revoke", "approval.request", "approval.approve", "role_binding.manage", "security_alert.read"},
	"PlatformAdmin": {"*"},
}

func (s *Service) Authorize(ctx context.Context, identityID uuid.UUID, action string, resource Resource, breakGlass bool) error {
	if identityID == uuid.Nil || resource.WorkspaceID == uuid.Nil || action == "" {
		return ErrForbidden
	}
	if breakGlass {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT role_name FROM role_bindings
		WHERE identity_id=$1 AND workspace_id=$2
		  AND (resource_type='workspace' OR (resource_type=$3 AND resource_id=$4)
		       OR (resource_type='node' AND resource_id=$4))`, identityID, resource.WorkspaceID, resource.Type, nullable(resource.ID))
	if err != nil {
		return fmt.Errorf("read role bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return err
		}
		allowed := roleActions[role]
		if slices.Contains(allowed, "*") || slices.Contains(allowed, action) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ErrForbidden
}

func (s *Service) Node(ctx context.Context, nodeID uuid.UUID) (Resource, error) {
	var resource Resource
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1`, nodeID).Scan(&resource.WorkspaceID)
	if err != nil {
		return Resource{}, err
	}
	resource.Type, resource.ID = "node", nodeID
	return resource, nil
}

func (s *Service) Operation(ctx context.Context, operationID uuid.UUID) (Resource, error) {
	var resource Resource
	var nodeID *uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM operations WHERE id=$1`, operationID).Scan(&resource.WorkspaceID, &nodeID)
	if err != nil {
		return Resource{}, err
	}
	resource.Type, resource.ID = "operation", operationID
	if nodeID != nil {
		resource.Type, resource.ID = "node", *nodeID
	}
	return resource, nil
}

func (s *Service) Workspace(ctx context.Context, workspaceID uuid.UUID) (Resource, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil {
		return Resource{}, err
	}
	if !exists {
		return Resource{}, pgx.ErrNoRows
	}
	return Resource{WorkspaceID: workspaceID, Type: "workspace"}, nil
}

func (s *Service) AuthorizedWorkspaces(ctx context.Context, identityID uuid.UUID, action string, breakGlass bool) ([]uuid.UUID, error) {
	var rows pgx.Rows
	var err error
	if breakGlass {
		rows, err = s.pool.Query(ctx, `SELECT id,'PlatformAdmin' FROM workspaces`)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT DISTINCT workspace_id,role_name FROM role_bindings WHERE identity_id=$1`, identityID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var workspaceID uuid.UUID
		var role string
		if err := rows.Scan(&workspaceID, &role); err != nil {
			return nil, err
		}
		allowed := roleActions[role]
		if slices.Contains(allowed, "*") || slices.Contains(allowed, action) {
			seen[workspaceID] = true
		}
	}
	result := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Service) CreateBinding(ctx context.Context, request BindingRequest) (uuid.UUID, error) {
	if request.IdentityID == uuid.Nil || request.WorkspaceID == uuid.Nil || request.ActorID == uuid.Nil || request.SessionID == uuid.Nil || request.RequestID == "" || request.Reason == "" ||
		!slices.Contains([]string{"Viewer", "Operator", "UserManager", "ConfigManager", "Auditor", "SecurityAdmin", "PlatformAdmin"}, request.Role) ||
		!slices.Contains([]string{"workspace", "node", "resource"}, request.ResourceType) || (request.ResourceType == "workspace") != (request.ResourceID == uuid.Nil) {
		return uuid.Nil, errors.New("role binding is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	id := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, request.IdentityID, request.WorkspaceID, request.Role, request.ResourceType, nullable(request.ResourceID), request.ActorID, now); err != nil {
		return uuid.Nil, err
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "role_binding.create", ResourceType: "role_binding", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func nullable(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
