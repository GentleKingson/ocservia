package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type createApprovalRequest struct {
	Action       string                             `json:"action"`
	ResourceType string                             `json:"resource_type"`
	ResourceID   string                             `json:"resource_id"`
	Reason       string                             `json:"reason"`
	TTLSeconds   int64                              `json:"ttl_seconds"`
	BatchItems   []useroperations.BatchItemRequest  `json:"batch_items,omitempty"`
	NodeApproval *nodeApprovalBindingRequest        `json:"node_approval,omitempty"`
	Certificate  *certificateApprovalBindingRequest `json:"certificate,omitempty"`
	RoleBinding  *roleBindingApprovalRequest        `json:"role_binding,omitempty"`
}

type nodeApprovalBindingRequest struct {
	Labels       map[string]string `json:"labels,omitempty"`
	Policy       string            `json:"policy"`
	Capabilities []string          `json:"capabilities"`
}
type certificateApprovalBindingRequest struct {
	ExpectedVersion   int64  `json:"expected_version"`
	Purpose           string `json:"purpose,omitempty"`
	ArtifactRequestID string `json:"artifact_request_id,omitempty"`
	Reason            string `json:"reason"`
}
type roleBindingApprovalRequest struct {
	IdentityID   string `json:"identity_id"`
	Role         string `json:"role"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
}

type approvalDecision struct {
	Reason              string `json:"reason"`
	ExpectedRequestHash string `json:"expected_request_hash,omitempty"`
}

func (s *Server) createApproval(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "approval service is unavailable")
		return
	}
	var body createApprovalRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	action := strings.TrimSpace(body.Action)
	var resourceID uuid.UUID
	var err error
	if action == "user.batch.disable" {
		if strings.TrimSpace(body.ResourceID) != "" {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "batch resource identifiers are server generated")
			return
		}
		resourceID = uuid.Must(uuid.NewV7())
	} else {
		resourceID, err = parseUUIDv7(body.ResourceID)
	}
	if err != nil || body.TTLSeconds < 60 || body.TTLSeconds > 86400 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "resource_id and ttl_seconds are invalid")
		return
	}
	resource := rbac.Resource{WorkspaceID: workspace(r), Type: strings.TrimSpace(body.ResourceType), ID: resourceID}
	actor := principal(r)
	var requestHash []byte
	var requestSummary json.RawMessage
	authorityResources := []approvals.AuthorityResource{}
	boundBatchItems := []approvals.BoundBatchItem{}
	if action == "user.batch.disable" {
		if resource.Type != "batch_operation" || useroperations.ValidateBatchItems(body.BatchItems) != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "batch approval requires the complete valid batch item set")
			return
		}
		allowed := true
		for _, item := range body.BatchItems {
			node, nodeErr := s.rbac.Node(r.Context(), item.NodeID)
			if nodeErr != nil || node.WorkspaceID != resource.WorkspaceID {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "every batch item must reference the selected workspace")
				return
			}
			if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "user.manage", node, actor.BreakGlass) != nil {
				allowed = false
			}
			authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: node.WorkspaceID, Type: "node", ID: item.NodeID})
			boundBatchItems = append(boundBatchItems, approvals.BoundBatchItem{NodeID: item.NodeID, Username: item.Username, Action: item.Action, ExpectedVersion: item.ExpectedVersion})
		}
		if !allowed {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
		hash := useroperations.BatchRequestHash(body.BatchItems)
		requestHash = hash[:]
		requestSummary, _ = json.Marshal(body.BatchItems)
	} else if action == "node.approve" {
		if resource.Type != "node" || body.NodeApproval == nil || s.enrollment == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node approval requires exact proposed trust content")
			return
		}
		workspaceID, hash, summary, bindingErr := s.enrollment.ApprovalBinding(r.Context(), resourceID, body.NodeApproval.Labels, body.NodeApproval.Policy, body.NodeApproval.Capabilities)
		if bindingErr != nil || workspaceID != resource.WorkspaceID {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/node-not-ready", "Node is not ready", "node approval content does not match a pending enrollment")
			return
		}
		requestHash, requestSummary = hash, summary
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: workspaceID, Type: "node", ID: resourceID})
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "node.approve", rbac.Resource{WorkspaceID: workspaceID, Type: "node", ID: resourceID}, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
	} else if action == "config.apply" {
		if resource.Type != "config_plan" || s.configplans == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "configuration approval requires an existing validated plan")
			return
		}
		plan, planErr := s.configplans.Get(r.Context(), resourceID)
		if planErr != nil || plan.Validation != "valid" || !plan.ExpiresAt.After(time.Now().UTC()) {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/config-plan-not-ready", "Configuration plan is not ready", "the plan must be valid and unexpired before approval")
			return
		}
		if plan.WorkspaceID != resource.WorkspaceID {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "the plan is outside the selected workspace")
			return
		}
		node := rbac.Resource{WorkspaceID: plan.WorkspaceID, Type: "node", ID: plan.NodeID}
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: plan.WorkspaceID, Type: "node", ID: plan.NodeID})
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "config.apply", node, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
		requestHash, _ = hex.DecodeString(plan.CandidateHash)
		requestSummary, _ = json.Marshal(map[string]any{"node_id": plan.NodeID, "expected_revision": plan.ExpectedRevision, "candidate_hash": plan.CandidateHash, "current_hash": plan.CurrentHash, "diff_redacted": plan.DiffRedacted, "expires_at": plan.ExpiresAt})
	} else if action == "certificate.issue" {
		if resource.Type != "certificate" || s.certificates == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "certificate approval requires a ready CSR")
			return
		}
		workspaceID, nodeID, hash, summary, bindingErr := s.certificates.ApprovalBinding(r.Context(), resourceID)
		if bindingErr != nil || workspaceID != resource.WorkspaceID {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/certificate-not-ready", "Certificate is not ready", "certificate approval requires a ready CSR in the selected workspace")
			return
		}
		node := rbac.Resource{WorkspaceID: workspaceID, Type: "node", ID: nodeID}
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: workspaceID, Type: "node", ID: nodeID})
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "certificate.issue", node, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
		requestHash, requestSummary = hash, summary
	} else if action == "certificate.revoke" || action == "certificate.private_key.export" {
		if resource.Type != "certificate" || body.Certificate == nil || s.certificates == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "certificate approval requires exact action content")
			return
		}
		artifactID := uuid.Nil
		if body.Certificate.ArtifactRequestID != "" {
			artifactID, err = parseUUIDv7(body.Certificate.ArtifactRequestID)
			if err != nil {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "artifact_request_id must be UUIDv7")
				return
			}
		}
		purpose := body.Certificate.Purpose
		if action == "certificate.revoke" {
			purpose = body.Certificate.Reason
		}
		workspaceID, nodeID, hash, summary, bindingErr := s.certificates.ActionApprovalBinding(r.Context(), action, resourceID, body.Certificate.ExpectedVersion, purpose, artifactID)
		if bindingErr != nil || workspaceID != resource.WorkspaceID {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/certificate-not-ready", "Certificate is not ready", "certificate content changed before approval")
			return
		}
		node := rbac.Resource{WorkspaceID: workspaceID, Type: "node", ID: nodeID}
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, action, node, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
		requestHash, requestSummary = hash, summary
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: workspaceID, Type: "node", ID: nodeID})
	} else if action == "role_binding.elevate" {
		if resource.Type != "role_binding" || body.RoleBinding == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "role elevation approval requires exact binding content")
			return
		}
		identityID, parseErr := parseUUIDv7(body.RoleBinding.IdentityID)
		if parseErr != nil || identityID != resourceID {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "role binding identity does not match resource_id")
			return
		}
		bindingResourceID := uuid.Nil
		if body.RoleBinding.ResourceID != "" {
			bindingResourceID, parseErr = parseUUIDv7(body.RoleBinding.ResourceID)
			if parseErr != nil {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "role binding resource is invalid")
				return
			}
		}
		if !slices.Contains([]string{"SecurityAdmin", "PlatformAdmin"}, body.RoleBinding.Role) {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "only high privilege roles use elevation approval")
			return
		}
		requestHash, requestSummary = rbac.BindingApprovalContent(identityID, resource.WorkspaceID, body.RoleBinding.Role, body.RoleBinding.ResourceType, bindingResourceID)
		scopeType, scopeID := body.RoleBinding.ResourceType, bindingResourceID
		if scopeType == "workspace" {
			scopeID = uuid.Nil
		}
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: resource.WorkspaceID, Type: scopeType, ID: scopeID})
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "role_binding.manage", rbac.Resource{WorkspaceID: resource.WorkspaceID, Type: scopeType, ID: scopeID}, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
	} else {
		requestHash, requestSummary = approvals.GenericBinding(action, resource.Type, resource.ID)
		authorityID := resource.ID
		if resource.Type == "workspace" {
			authorityID = uuid.Nil
		}
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: resource.WorkspaceID, Type: resource.Type, ID: authorityID})
	}
	// Approval requests cannot be used to ask another user to authorize an action
	// that the requester could not perform themselves.
	if !s.devAuth && action != "user.batch.disable" && action != "config.apply" && action != "certificate.issue" && action != "certificate.revoke" && action != "certificate.private_key.export" && action != "node.approve" && action != "role_binding.elevate" {
		if err := s.rbac.Authorize(r.Context(), actor.IdentityID, action, resource, actor.BreakGlass); err != nil {
			s.writeAuthorizationError(w, r, err)
			return
		}
	}
	value, err := s.approvals.Create(r.Context(), approvals.Request{WorkspaceID: resource.WorkspaceID, RequesterID: actor.IdentityID, ResourceID: resourceID, Action: action, ResourceType: body.ResourceType, Reason: body.Reason, TTL: time.Duration(body.TTLSeconds) * time.Second, SessionID: actor.SessionID, RequestID: requestID(r), RequestHash: requestHash, RequestSummary: requestSummary, AuthorityResources: authorityResources, BatchItems: boundBatchItems})
	if err != nil {
		writeApprovalError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/approval-requests/"+value.ID.String())
	writeJSON(w, http.StatusCreated, value)
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(strings.TrimSuffix(r.PathValue("approval_id"), ":approve"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	var body approvalDecision
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	value, err := s.approvals.Approve(r.Context(), approvals.Decision{ApprovalID: id, ApproverID: actor.IdentityID, SessionID: actor.SessionID, Reason: body.Reason, RequestID: requestID(r), ExpectedRequestHash: strings.TrimSpace(body.ExpectedRequestHash)})
	if err != nil {
		writeApprovalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("approval_id"))
	if err != nil || id.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	value, err := s.approvals.Get(r.Context(), id)
	if err != nil {
		writeApprovalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeApprovalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, approvals.ErrSelf):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/self-approval-forbidden", "Self-approval forbidden", "the requester cannot approve their own request")
	case errors.Is(err, approvals.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/approval-not-ready", "Approval unavailable", "the approval is expired, consumed, or does not match")
	case errors.Is(err, approvals.ErrInvalid):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "the approval request is invalid")
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the approval does not exist")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Approval unavailable", "approval state is temporarily unavailable")
	}
}
