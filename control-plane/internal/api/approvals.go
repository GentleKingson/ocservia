package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type createApprovalRequest struct {
	Action       string                            `json:"action"`
	ResourceType string                            `json:"resource_type"`
	ResourceID   string                            `json:"resource_id"`
	Reason       string                            `json:"reason"`
	TTLSeconds   int64                             `json:"ttl_seconds"`
	BatchItems   []useroperations.BatchItemRequest `json:"batch_items,omitempty"`
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
	if !decodeStrict(w, r, &body) {
		return
	}
	resourceID, err := parseUUIDv7(body.ResourceID)
	if err != nil || body.TTLSeconds < 60 || body.TTLSeconds > 86400 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "resource_id and ttl_seconds are invalid")
		return
	}
	resource := rbac.Resource{WorkspaceID: workspace(r), Type: strings.TrimSpace(body.ResourceType), ID: resourceID}
	actor := principal(r)
	var requestHash []byte
	var requestSummary json.RawMessage
	if strings.TrimSpace(body.Action) == "user.batch.disable" {
		if resource.Type != "batch_operation" || useroperations.ValidateBatchItems(body.BatchItems) != nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "batch approval requires the complete valid batch item set")
			return
		}
		allowed := false
		for _, item := range body.BatchItems {
			node, nodeErr := s.rbac.Node(r.Context(), item.NodeID)
			if nodeErr != nil || node.WorkspaceID != resource.WorkspaceID {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "every batch item must reference the selected workspace")
				return
			}
			if s.devAuth || s.rbac.Authorize(r.Context(), actor.IdentityID, "user.manage", node, actor.BreakGlass) == nil {
				allowed = true
			}
		}
		if !allowed {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
		hash := useroperations.BatchRequestHash(body.BatchItems)
		requestHash = hash[:]
		requestSummary, _ = json.Marshal(body.BatchItems)
	}
	// Approval requests cannot be used to ask another user to authorize an action
	// that the requester could not perform themselves.
	if !s.devAuth && strings.TrimSpace(body.Action) != "user.batch.disable" {
		if err := s.rbac.Authorize(r.Context(), actor.IdentityID, strings.TrimSpace(body.Action), resource, actor.BreakGlass); err != nil {
			s.writeAuthorizationError(w, r, err)
			return
		}
	}
	value, err := s.approvals.Create(r.Context(), approvals.Request{WorkspaceID: resource.WorkspaceID, RequesterID: actor.IdentityID, ResourceID: resourceID, Action: body.Action, ResourceType: body.ResourceType, Reason: body.Reason, TTL: time.Duration(body.TTLSeconds) * time.Second, SessionID: actor.SessionID, RequestID: requestID(r), RequestHash: requestHash, RequestSummary: requestSummary})
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
	if !decodeStrict(w, r, &body) {
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
