package rbac

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac/rbacstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrForbidden      = errors.New("authorization denied")
	ErrGrantForbidden = errors.New("role grant exceeds actor permissions")
)

type Resource struct {
	WorkspaceID uuid.UUID
	Type        string
	ID          uuid.UUID
}

type Service struct{ backend database.Backend }

type BindingRequest struct {
	IdentityID, WorkspaceID, ResourceID, ActorID, SessionID, ApprovalID uuid.UUID
	Role, ResourceType, RequestID, Reason                               string
}

func New(pool *pgxpool.Pool) *Service              { return NewBackend(postgres.WrapPool(pool)) }
func NewBackend(backend database.Backend) *Service { return &Service{backend: backend} }

var roleActions = map[string][]string{
	"Viewer":        {"node.read", "operation.read"},
	"Operator":      {"node.read", "operation.read", "operation.create", "session.disconnect", "session.terminate", "ip_ban.remove", "service.reload", "agent.upgrade", "approval.request"},
	"UserManager":   {"node.read", "operation.read", "user.manage", "user.batch.disable", "group.manage", "approval.request"},
	"ConfigManager": {"node.read", "operation.read", "config.plan", "config.review", "config.apply", "certificate.read", "certificate.issue", "secret.use", "approval.request"},
	"Auditor":       {"node.read", "operation.read", "audit.read", "audit.verify"},
	"SecurityAdmin": {"node.read", "operation.read", "config.review", "certificate.read", "certificate.revoke", "certificate.private_key.export", "secret.read", "secret.use", "secret.manage", "enrollment_token.create", "node_bootstrap_token.create", "node.approve", "node.revoke", "privd.attestation.manage", "approval.request", "approval.approve", "role_binding.manage", "security_alert.read"},
	"PlatformAdmin": {"*"},
}

func (s *Service) Authorize(ctx context.Context, identityID uuid.UUID, action string, resource Resource, breakGlass bool) error {
	if identityID == uuid.Nil || resource.WorkspaceID == uuid.Nil || action == "" {
		return ErrForbidden
	}
	if breakGlass {
		return nil
	}
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return err
	}
	rows, err := store.Roles(ctx, identityID, resource.WorkspaceID, resource.Type, resource.ID)
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
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return Resource{}, err
	}
	err = store.Node(ctx, nodeID).Scan(&resource.WorkspaceID)
	if err != nil {
		return Resource{}, err
	}
	resource.Type, resource.ID = "node", nodeID
	return resource, nil
}

func (s *Service) Operation(ctx context.Context, operationID uuid.UUID) (Resource, error) {
	var resource Resource
	var nodeID *uuid.UUID
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return Resource{}, err
	}
	err = store.Operation(ctx, operationID).Scan(&resource.WorkspaceID, &nodeID)
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
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return Resource{}, err
	}
	if err := store.Workspace(ctx, workspaceID).Scan(&exists); err != nil {
		return Resource{}, err
	}
	if !exists {
		return Resource{}, database.ErrNotFound
	}
	return Resource{WorkspaceID: workspaceID, Type: "workspace"}, nil
}

func (s *Service) AuthorizedWorkspaces(ctx context.Context, identityID uuid.UUID, action string, breakGlass bool) ([]uuid.UUID, error) {
	store, err := rbacstore.From(s.backend)
	if err != nil {
		return nil, err
	}
	rows, err := store.AuthorizedWorkspaces(ctx, identityID, breakGlass)
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
		!slices.Contains([]string{"workspace", "node", "resource", "secret_ref", "certificate", "config_plan", "batch_operation", "role_binding"}, request.ResourceType) || (request.ResourceType == "workspace") != (request.ResourceID == uuid.Nil) {
		return uuid.Nil, errors.New("role binding is invalid")
	}
	id := uuid.Must(uuid.NewV7())
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := rbacstore.From(tx)
		if err != nil {
			return err
		}
		// Serialize with one-shot completion and management account protection.
		if err := store.LockManagement(ctx); err != nil {
			return err
		}
		allowed, err := canGrantRole(ctx, tx, request.ActorID, request.WorkspaceID, request.Role)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrGrantForbidden
		}
		if slices.Contains([]string{"SecurityAdmin", "PlatformAdmin"}, request.Role) {
			hash, _ := BindingApprovalContent(request.IdentityID, request.WorkspaceID, request.Role, request.ResourceType, request.ResourceID)
			if err := approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, request.WorkspaceID, request.ActorID, "role_binding.elevate", "role_binding", request.IdentityID, hash); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if err := store.Insert(ctx, rbacstore.Binding{ID: id, IdentityID: request.IdentityID, WorkspaceID: request.WorkspaceID, Role: request.Role, ResourceType: request.ResourceType, ResourceID: request.ResourceID, ActorID: request.ActorID, At: now, ApprovalID: request.ApprovalID}); err != nil {
			return err
		}
		if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: request.WorkspaceID, ActorType: "user", ActorID: request.ActorID.String(), SessionID: &request.SessionID, Action: "role_binding.create", ResourceType: "role_binding", ResourceID: id, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, At: now}); err != nil {
			return err
		}
		if slices.Contains([]string{"SecurityAdmin", "PlatformAdmin"}, request.Role) {
			if err := store.CompleteBootstrap(ctx, request.WorkspaceID, request.IdentityID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func BindingApprovalContent(identityID, workspaceID uuid.UUID, role, resourceType string, resourceID uuid.UUID) ([]byte, json.RawMessage) {
	summary, _ := json.Marshal(struct {
		IdentityID   uuid.UUID  `json:"identity_id"`
		WorkspaceID  uuid.UUID  `json:"workspace_id"`
		Role         string     `json:"role"`
		ResourceType string     `json:"resource_type"`
		ResourceID   *uuid.UUID `json:"resource_id,omitempty"`
	}{identityID, workspaceID, role, resourceType, optionalID(resourceID)})
	digest := sha256.Sum256(append([]byte("ocservia/role-binding-approval/v1\x00"), summary...))
	return digest[:], summary
}

func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func canGrantRole(ctx context.Context, tx database.Tx, actorID, workspaceID uuid.UUID, role string) (bool, error) {
	store, err := rbacstore.From(tx)
	if err != nil {
		return false, err
	}
	rows, err := store.WorkspaceRoles(ctx, actorID, workspaceID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	actorActions := map[string]bool{}
	for rows.Next() {
		var actorRole string
		if err := rows.Scan(&actorRole); err != nil {
			return false, err
		}
		for _, action := range roleActions[actorRole] {
			actorActions[action] = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if actorActions["*"] {
		return true, nil
	}
	for _, action := range roleActions[role] {
		if action == "*" || !actorActions[action] {
			return false, nil
		}
	}
	return true, nil
}
