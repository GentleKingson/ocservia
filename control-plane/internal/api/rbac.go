package api

import (
	"errors"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
)

type roleBindingRequest struct {
	IdentityID   string `json:"identity_id"`
	WorkspaceID  string `json:"workspace_id"`
	Role         string `json:"role"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	Reason       string `json:"reason"`
	ApprovalID   string `json:"approval_id,omitempty"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	actor := principal(r)
	ids, err := s.rbac.AuthorizedWorkspaces(r.Context(), actor.IdentityID, "node.read", actor.BreakGlass)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id,name,slug,version FROM workspaces WHERE id=ANY($1::uuid[]) ORDER BY name,id`, ids)
	if err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id uuid.UUID
		var name, slug string
		var version int64
		if err := rows.Scan(&id, &name, &slug, &version); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Workspaces unavailable", "authorized workspaces could not be read")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "slug": slug, "version": version})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createRoleBinding(w http.ResponseWriter, r *http.Request) {
	var body roleBindingRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	identityID, err := parseUUIDv7(body.IdentityID)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "identity_id must be UUIDv7")
		return
	}
	workspaceID, err := parseUUIDv7(body.WorkspaceID)
	if err != nil || workspaceID != workspace(r) {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "workspace_id is outside the authorized scope")
		return
	}
	resourceID := uuid.Nil
	if body.ResourceID != "" {
		resourceID, err = parseUUIDv7(body.ResourceID)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "resource_id must be UUIDv7")
			return
		}
	}
	actor := principal(r)
	approvalID := uuid.Nil
	if body.ApprovalID != "" {
		approvalID, err = parseUUIDv7(body.ApprovalID)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "approval_id must be UUIDv7")
			return
		}
	}
	id, err := s.rbac.CreateBinding(r.Context(), rbac.BindingRequest{IdentityID: identityID, WorkspaceID: workspaceID, ResourceID: resourceID, ActorID: actor.IdentityID, SessionID: actor.SessionID, ApprovalID: approvalID, Role: body.Role, ResourceType: body.ResourceType, RequestID: requestID(r), Reason: body.Reason})
	if err != nil {
		if errors.Is(err, rbac.ErrGrantForbidden) {
			writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the requested role exceeds the actor's effective permissions")
			return
		}
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "the role binding could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}
