package api

import (
	"encoding/base64"
	"errors"
	"net/http"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
)

func (s *Server) admitInstallation(w http.ResponseWriter, r *http.Request) func() {
	w.Header().Set("Cache-Control", "no-store")
	if s.enrollment == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "node installation is not enabled")
		return nil
	}
	return s.admitAuthentication(w, r, s.privdRegisterBudget)
}

func (s *Server) installationConfig(w http.ResponseWriter, r *http.Request) {
	release := s.admitInstallation(w, r)
	if release == nil {
		return
	}
	defer release()
	var body struct {
		Token string `json:"token"`
	}
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	config, err := s.enrollment.InstallationConfig(r.Context(), body.Token)
	if err != nil {
		s.writeInstallationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) installationAttestation(w http.ResponseWriter, r *http.Request) {
	release := s.admitInstallation(w, r)
	if release == nil {
		return
	}
	defer release()
	var body enrollment.InstallationBinding
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	challenge, err := s.enrollment.InstallationChallenge(r.Context(), body)
	if err != nil {
		s.writeInstallationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

func (s *Server) installationStatus(w http.ResponseWriter, r *http.Request) {
	release := s.admitInstallation(w, r)
	if release == nil {
		return
	}
	defer release()
	var body enrollment.InstallationBinding
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	challenge, err := s.enrollment.InstallationChallenge(r.Context(), body)
	if err != nil {
		s.writeInstallationError(w, r, err)
		return
	}
	node, err := telemetry.NewBackend(s.backend).GetNode(r.Context(), body.NodeID)
	if err != nil {
		s.writeInstallationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trust_status": node.TrustStatus, "connection_state": node.ConnectionState, "freshness": node.Freshness, "attestation_registered": challenge.Registered})
}

func (s *Server) installationRegister(w http.ResponseWriter, r *http.Request) {
	release := s.admitInstallation(w, r)
	if release == nil {
		return
	}
	defer release()
	var body struct {
		enrollment.InstallationKeyBinding
		Registration struct {
			Version                 uint32 `json:"version"`
			KeyID                   string `json:"key_id"`
			PublicKey               string `json:"public_key"`
			ControllerNonce         string `json:"controller_nonce"`
			CredentialContextSHA256 string `json:"credential_context_sha256"`
			Signature               string `json:"signature"`
		} `json:"registration"`
	}
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	decode := func(encoded string) ([]byte, bool) {
		value, err := base64.RawURLEncoding.DecodeString(encoded)
		return value, err == nil && base64.RawURLEncoding.EncodeToString(value) == encoded
	}
	public, ok1 := decode(body.Registration.PublicKey)
	nonce, ok2 := decode(body.Registration.ControllerNonce)
	context, ok3 := decode(body.Registration.CredentialContextSHA256)
	signature, ok4 := decode(body.Registration.Signature)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		s.writeInstallationError(w, r, enrollment.ErrInvalidRequest)
		return
	}
	id, err := s.enrollment.RegisterInstallationKey(r.Context(), body.InstallationKeyBinding, &agentv1.PrivdAttestationRegistrationV1{Version: body.Registration.Version, NodeId: body.NodeID[:], PrivdAttestationKeyId: body.Registration.KeyID, PublicKey: public, ControllerNonce: nonce, CredentialContextSha256: context, Signature: signature}, requestID(r))
	if err != nil {
		s.writeInstallationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key_id": id, "state": "active"})
}

func (s *Server) writeInstallationError(w http.ResponseWriter, r *http.Request, err error) {
	status, detail := http.StatusServiceUnavailable, "node installation is temporarily unavailable"
	if errors.Is(err, enrollment.ErrInvalidToken) || errors.Is(err, enrollment.ErrEndpointMismatch) {
		status, detail = http.StatusUnauthorized, "installation token is invalid, expired, or does not match this node"
	}
	if errors.Is(err, enrollment.ErrInvalidRequest) {
		status, detail = http.StatusBadRequest, "installation request is invalid"
	}
	writeProblem(w, r, status, "https://ocservia.dev/problems/node-installation", "Node installation failed", detail)
}
