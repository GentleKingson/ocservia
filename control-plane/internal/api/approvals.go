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
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
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
	AgentUpgrade *agentUpgradeApprovalRequest       `json:"agent_upgrade,omitempty"`
	AgentRollout *agentRolloutApprovalRequest       `json:"agent_rollout,omitempty"`
}

// agentUpgradeApprovalRequest selects only the target version. The package
// digest and architecture resolve server-side from the node's observed state
// and the trusted release catalog so approval content is never caller-trusted.
type agentUpgradeApprovalRequest struct {
	TargetVersion string `json:"target_version"`
}

// agentRolloutApprovalRequest pins the immutable fleet rollout request: the
// target version, the sorted node set, the batch size, and the stop-on-failure
// policy. Eligibility and package identities still resolve server-side.
type agentRolloutApprovalRequest struct {
	TargetVersion string   `json:"target_version"`
	NodeIDs       []string `json:"node_ids"`
	BatchSize     int      `json:"batch_size"`
	StopOnFailure bool     `json:"stop_on_failure"`
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
	} else if action == "agent.rollout" {
		if strings.TrimSpace(body.ResourceID) != "" {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "rollout resource identifiers are server generated")
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
	} else if action == "agent.upgrade" {
		if resource.Type != "node" || body.AgentUpgrade == nil || s.releaseCatalog == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "agent upgrade approval requires a target version and a trusted release catalog")
			return
		}
		target := strings.TrimSpace(body.AgentUpgrade.TargetVersion)
		if !semanticpayload.ValidAgentUpgradeTargetVersion(target) {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "agent upgrade approval target version is invalid")
			return
		}
		var nodeWorkspace uuid.UUID
		var architecture string
		if err := s.pool.QueryRow(r.Context(), `SELECT n.workspace_id,COALESCE(o.architecture,'') FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1`, resourceID).Scan(&nodeWorkspace, &architecture); err != nil || nodeWorkspace != resource.WorkspaceID {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/node-not-ready", "Node is not ready", "agent upgrade approval requires an observed node in the selected workspace")
			return
		}
		if architecture == "" {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/node-not-ready", "Node is not ready", "the node has not reported its package architecture yet")
			return
		}
		digest, trusted := s.releaseCatalog.Lookup(target, architecture)
		if !trusted {
			writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/release-not-trusted", "Release is not trusted", "no trusted release exists for the requested version and architecture")
			return
		}
		requestHash, requestSummary = approvals.AgentUpgradeBinding(resourceID, target, digest[:], architecture)
		authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: resource.WorkspaceID, Type: "node", ID: resourceID})
		if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, action, rbac.Resource{WorkspaceID: resource.WorkspaceID, Type: "node", ID: resourceID}, actor.BreakGlass) != nil {
			s.writeAuthorizationError(w, r, rbac.ErrForbidden)
			return
		}
	} else if action == "agent.rollout" {
		if resource.Type != "batch_operation" || body.AgentRollout == nil || s.releaseCatalog == nil {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "agent rollout approval requires the exact rollout request and a trusted release catalog")
			return
		}
		target := strings.TrimSpace(body.AgentRollout.TargetVersion)
		if !semanticpayload.ValidAgentUpgradeTargetVersion(target) || !body.AgentRollout.StopOnFailure || body.AgentRollout.BatchSize < 1 || body.AgentRollout.BatchSize > 20 || len(body.AgentRollout.NodeIDs) == 0 || len(body.AgentRollout.NodeIDs) > 500 {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "agent rollout approval target version, node set, batch size, or stop-on-failure policy is invalid")
			return
		}
		nodeIDs := make([]uuid.UUID, 0, len(body.AgentRollout.NodeIDs))
		seen := make(map[uuid.UUID]struct{}, len(body.AgentRollout.NodeIDs))
		for _, raw := range body.AgentRollout.NodeIDs {
			nodeID, parseErr := uuid.Parse(strings.TrimSpace(raw))
			if parseErr != nil || nodeID.Version() != 7 {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "every rollout node_id must be a UUIDv7")
				return
			}
			if _, duplicate := seen[nodeID]; duplicate {
				continue
			}
			seen[nodeID] = struct{}{}
			nodeIDs = append(nodeIDs, nodeID)
		}
		slices.SortFunc(nodeIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		for _, nodeID := range nodeIDs {
			node, nodeErr := s.rbac.Node(r.Context(), nodeID)
			if nodeErr != nil || node.WorkspaceID != resource.WorkspaceID {
				writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "every rollout node must reference the selected workspace")
				return
			}
			if !s.devAuth && s.rbac.Authorize(r.Context(), actor.IdentityID, "agent.upgrade", node, actor.BreakGlass) != nil {
				s.writeAuthorizationError(w, r, rbac.ErrForbidden)
				return
			}
			authorityResources = append(authorityResources, approvals.AuthorityResource{WorkspaceID: node.WorkspaceID, Type: "node", ID: nodeID})
		}
		requestHash, requestSummary = approvals.AgentRolloutBinding(target, nodeIDs, body.AgentRollout.BatchSize, body.AgentRollout.StopOnFailure)
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
	if !s.devAuth && action != "user.batch.disable" && action != "config.apply" && action != "certificate.issue" && action != "certificate.revoke" && action != "certificate.private_key.export" && action != "node.approve" && action != "role_binding.elevate" && action != "agent.rollout" {
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
