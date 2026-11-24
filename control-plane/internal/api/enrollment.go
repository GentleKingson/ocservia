package api

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
)

type bootstrapTokenRequest struct {
	Installation       *bool  `json:"installation"`
	ExpectedEndpointID string `json:"expected_endpoint_id"`
	WorkspaceID        string `json:"workspace_id"`
	Environment        string `json:"environment"`
	ExpectedNodeName   string `json:"expected_node_name"`
	TTLSeconds         int64  `json:"ttl_seconds"`
	Reason             string `json:"reason"`
}

type revocationRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) createNodeBootstrapToken(w http.ResponseWriter, r *http.Request) {
	if s.enrollment == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "enrollment is not enabled")
		return
	}
	var body bootstrapTokenRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	if body.Installation != nil && !*body.Installation {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "installation tokens are required; obt1_ bootstrap tokens are retired")
		return
	}
	workspaceID, err := parseUUIDv7(body.WorkspaceID)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "workspace_id must be UUIDv7")
		return
	}
	if principal(r).Issuer != "development" && workspaceID != workspace(r) {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "workspace_id is outside the authorized scope")
		return
	}
	var endpoint []byte
	if body.ExpectedEndpointID != "" {
		var err error
		endpoint, err = hex.DecodeString(body.ExpectedEndpointID)
		if err != nil || len(endpoint) != 32 || strings.ToLower(body.ExpectedEndpointID) != body.ExpectedEndpointID {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "expected_endpoint_id must be 32-byte lowercase hex")
			return
		}
	}
	if !validEnrollmentTTLSeconds(body.TTLSeconds) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "ttl_seconds must be between 1 and 900")
		return
	}
	actor := principal(r)
	token, err := s.enrollment.CreateInstallationToken(r.Context(), enrollment.BootstrapTokenSpec{
		ExpectedEndpointID: endpoint, WorkspaceID: workspaceID, Environment: body.Environment,
		ExpectedNodeName: body.ExpectedNodeName, TTL: time.Duration(body.TTLSeconds) * time.Second,
		ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID,
		Reason: body.Reason, RequestID: requestID(r),
	})
	if err != nil {
		s.writeEnrollmentError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"id": token.ID, "token": token.Value, "expires_at": token.ExpiresAt})
}

func validEnrollmentTTLSeconds(seconds int64) bool {
	return seconds == 0 || seconds >= 1 && seconds <= int64(enrollment.DefaultTokenTTL/time.Second)
}

func (s *Server) revokeNode(w http.ResponseWriter, r *http.Request) {
	if s.enrollment == nil || s.transport == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "enrollment is not enabled")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node_id must be UUIDv7")
		return
	}
	var body revocationRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	trust, err := s.enrollment.Revoke(r.Context(), enrollment.Revocation{
		NodeID: nodeID, ActorID: actorID(r), IdentityID: actor.IdentityID, SessionID: actor.SessionID,
		Reason: body.Reason, RequestID: requestID(r),
	})
	if err != nil {
		s.writeEnrollmentError(w, r, err)
		return
	}
	syncErr := s.transport.UpdateNodeTrust(r.Context(), trust.NodeID[:], trust.EndpointID, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, body.Reason, trust.Revision)
	var closeErr error
	if syncErr == nil {
		closeErr = s.transport.CloseNode(r.Context(), trust.NodeID[:], "node revoked")
	}
	if syncErr == nil {
		if _, err := s.enrollment.RecordRevocationSynchronized(r.Context(), trust, body.Reason, closeErr == nil); err != nil {
			s.logger.WarnContext(r.Context(), "record synchronous revocation convergence", "error", err)
		}
	}
	if syncErr != nil || closeErr != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/transport-unavailable", "Revocation committed", "the node is revoked but transport disconnect synchronization is pending")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": nodeID, "status": "revoked"})
}

func (s *Server) writeEnrollmentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Forbidden", "the current session is not authorized for this operation")
	case errors.Is(err, enrollment.ErrInvalidTransition):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/invalid-node-state", "Invalid node state", "the requested node transition is not allowed")
	case errors.Is(err, enrollment.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
	case errors.Is(err, enrollment.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request could not be completed")
	default:
		s.logger.ErrorContext(r.Context(), "enrollment persistence failed", "error", err)
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Enrollment service unavailable", "the enrollment database is temporarily unavailable")
	}
}
