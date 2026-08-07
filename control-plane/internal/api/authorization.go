package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type principalKey struct{}
type workspaceKey struct{}

func (s *Server) authenticate(r *http.Request) (auth.Principal, error) {
	if s.hasOperationPrincipal(r) {
		return auth.Principal{Subject: "developer", Issuer: "development", BreakGlass: true}, nil
	}
	if s.auth == nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return s.auth.Authenticate(r.Context(), cookie)
}

func (s *Server) authorizeRoute(r *http.Request, principal auth.Principal) (context.Context, error) {
	if r.URL.Path == "/api/v1/auth/logout" {
		return context.WithValue(r.Context(), principalKey{}, principal), nil
	}
	if r.URL.Path == "/api/v1/workspaces" {
		return context.WithValue(r.Context(), principalKey{}, principal), nil
	}
	if principal.Issuer == "development" {
		return context.WithValue(r.Context(), principalKey{}, principal), nil
	}
	action := routeAction(r)
	if action == "" {
		return nil, rbac.ErrForbidden
	}
	if (r.Method == http.MethodPost && (r.URL.Path == "/api/v1/user-batches" || r.URL.Path == "/api/v1/approval-requests")) || (r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/user-batches/")) {
		workspaceID, err := s.selectWorkspace(r, principal, action)
		if err != nil {
			return nil, err
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		return context.WithValue(ctx, workspaceKey{}, workspaceID), nil
	}
	var resource rbac.Resource
	var err error
	if nodeText := r.PathValue("node_id"); nodeText != "" {
		nodeID, parseErr := uuid.Parse(nodeText)
		if parseErr != nil || nodeID.Version() != 7 {
			return nil, pgx.ErrNoRows
		}
		resource, err = s.rbac.Node(r.Context(), nodeID)
	} else if operationText := r.PathValue("operation_id"); operationText != "" {
		operationID, parseErr := uuid.Parse(operationText)
		if parseErr != nil || operationID.Version() != 7 {
			return nil, pgx.ErrNoRows
		}
		resource, err = s.rbac.Operation(r.Context(), operationID)
	} else if approvalText := r.PathValue("approval_id"); approvalText != "" {
		approvalID, parseErr := uuid.Parse(strings.TrimSuffix(approvalText, ":approve"))
		if parseErr != nil || approvalID.Version() != 7 || s.approvals == nil {
			return nil, pgx.ErrNoRows
		}
		approval, getErr := s.approvals.Get(r.Context(), approvalID)
		if getErr != nil {
			return nil, getErr
		}
		resource = rbac.Resource{WorkspaceID: approval.WorkspaceID, Type: approval.ResourceType, ID: approval.ResourceID}
	} else {
		workspaceText := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
		if workspaceText == "" && r.URL.Path == "/api/v1/events/stream" {
			workspaceText = strings.TrimSpace(r.URL.Query().Get("workspace_id"))
		}
		if workspaceText == "" {
			workspaceIDs, listErr := s.rbac.AuthorizedWorkspaces(r.Context(), principal.IdentityID, action, principal.BreakGlass)
			if listErr != nil {
				return nil, listErr
			}
			if len(workspaceIDs) != 1 {
				return nil, rbac.ErrForbidden
			}
			workspaceText = workspaceIDs[0].String()
		}
		workspaceID, parseErr := uuid.Parse(workspaceText)
		if parseErr != nil || workspaceID.Version() != 7 {
			return nil, rbac.ErrForbidden
		}
		resource, err = s.rbac.Workspace(r.Context(), workspaceID)
	}
	if err != nil {
		return nil, err
	}
	if err := s.rbac.Authorize(r.Context(), principal.IdentityID, action, resource, principal.BreakGlass); err != nil {
		return nil, err
	}
	ctx := context.WithValue(r.Context(), principalKey{}, principal)
	ctx = context.WithValue(ctx, workspaceKey{}, resource.WorkspaceID)
	return ctx, nil
}

func (s *Server) selectWorkspace(r *http.Request, principal auth.Principal, action string) (uuid.UUID, error) {
	workspaceText := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
	workspaceIDs, err := s.rbac.AuthorizedWorkspaces(r.Context(), principal.IdentityID, action, principal.BreakGlass)
	if err != nil {
		return uuid.Nil, err
	}
	if workspaceText == "" {
		if len(workspaceIDs) != 1 {
			return uuid.Nil, rbac.ErrForbidden
		}
		workspaceText = workspaceIDs[0].String()
	}
	workspaceID, parseErr := uuid.Parse(workspaceText)
	if parseErr != nil || workspaceID.Version() != 7 || !slices.Contains(workspaceIDs, workspaceID) {
		return uuid.Nil, rbac.ErrForbidden
	}
	if _, err := s.rbac.Workspace(r.Context(), workspaceID); err != nil {
		return uuid.Nil, err
	}
	return workspaceID, nil
}

func routeAction(r *http.Request) string {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, ":disconnect"):
		return "session.disconnect"
	case strings.HasSuffix(path, ":terminate"):
		return "session.terminate"
	case strings.Contains(path, "/ip-bans/") && strings.HasSuffix(path, ":remove"):
		return "ip_ban.remove"
	case strings.HasSuffix(path, "/service:reload"):
		return "service.reload"
	case strings.HasSuffix(path, ":disable"), strings.HasSuffix(path, ":enable"), strings.HasSuffix(path, ":rotate-password"), strings.HasSuffix(path, "/users"):
		return "user.manage"
	case strings.Contains(path, "/users/") && strings.HasSuffix(path, "/policy"):
		if r.Method == http.MethodPut {
			return "user.manage"
		}
		return "node.read"
	case path == "/api/v1/user-batches":
		return "user.manage"
	case strings.HasPrefix(path, "/api/v1/user-batches/"):
		return "operation.read"
	case path == "/api/v1/user-operations/metrics":
		return "operation.read"
	case strings.Contains(path, "/groups/"):
		return "group.manage"
	case strings.HasSuffix(path, "/approval"):
		return "node.approve"
	case strings.HasSuffix(path, "/revocation"):
		return "node.revoke"
	case strings.HasSuffix(path, ":approve") && strings.Contains(path, "/approval-requests/"):
		return "approval.approve"
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/approval-requests/"):
		return "approval.approve"
	case path == "/api/v1/approval-requests":
		return "approval.request"
	case strings.HasPrefix(path, "/api/v1/audit"):
		if strings.HasSuffix(path, ":verify") {
			return "audit.verify"
		}
		return "audit.read"
	case path == "/api/v1/role-bindings":
		return "role_binding.manage"
	case path == "/api/v1/enrollment-tokens":
		return "enrollment_token.create"
	case strings.HasPrefix(path, "/api/v1/operations"):
		return "operation.read"
	case strings.HasPrefix(path, "/api/v1/events"):
		return "operation.read"
	case strings.HasPrefix(path, "/api/v1/nodes"):
		return "node.read"
	default:
		return ""
	}
}

func principal(r *http.Request) auth.Principal {
	value, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return value
}

func workspace(r *http.Request) uuid.UUID {
	value, _ := r.Context().Value(workspaceKey{}).(uuid.UUID)
	return value
}

func (s *Server) writeAuthorizationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the principal is not authorized for this resource and action")
}
