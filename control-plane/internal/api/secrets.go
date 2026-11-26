package api

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
)

type secretRefRequest struct {
	Provider string `json:"provider"`
	KeyPath  string `json:"key_path"`
	Version  string `json:"version"`
	Reason   string `json:"reason"`
}
type secretRotateRequest struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

func (s *Server) createSecretRef(w http.ResponseWriter, r *http.Request) {
	if !s.requireCertificates(w, r) {
		return
	}
	var body secretRefRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	value, err := s.certificates.CreateSecretRef(r.Context(), certificates.SecretRefRequest{WorkspaceID: workspace(r), ActorID: actor.IdentityID, SessionID: actor.SessionID, Provider: body.Provider, KeyPath: body.KeyPath, Version: body.Version, Reason: body.Reason, RequestID: requestID(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/secret-provider-refs/"+value.ID.String())
	writeJSON(w, http.StatusCreated, value)
}
func (s *Server) getSecretRef(w http.ResponseWriter, r *http.Request) {
	if !s.requireCertificates(w, r) {
		return
	}
	id, ok := requirePathUUIDv7(w, r, "secret_ref_id", "")
	if !ok {
		return
	}
	value, err := s.certificates.GetSecretRef(r.Context(), id)
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
func (s *Server) rotateSecretRef(w http.ResponseWriter, r *http.Request) {
	if !s.requireCertificates(w, r) {
		return
	}
	id, ok := requirePathUUIDv7(w, r, "secret_ref_id", ":rotate")
	if !ok {
		return
	}
	var body secretRotateRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	value, err := s.certificates.RotateSecretRef(r.Context(), id, certificates.SecretRefRequest{ActorID: actor.IdentityID, SessionID: actor.SessionID, Version: body.Version, Reason: body.Reason, RequestID: requestID(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
