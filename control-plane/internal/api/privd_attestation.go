package api

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
)

type privdCredentialRequest struct {
	TTLSeconds int64  `json:"ttl_seconds"`
	Reason     string `json:"reason"`
}

type privdRegistrationRequest struct {
	Credential              string `json:"credential"`
	Version                 int32  `json:"version"`
	KeyID                   string `json:"key_id"`
	PublicKey               string `json:"public_key"`
	ControllerNonce         string `json:"controller_nonce"`
	CredentialContextSHA256 string `json:"credential_context_sha256"`
	Signature               string `json:"signature"`
}

type privdRevokeRequest struct {
	KeyID  string `json:"key_id"`
	Reason string `json:"reason"`
}

func (s *Server) createPrivdAttestationCredential(w http.ResponseWriter, r *http.Request) {
	if s.privdAttestation == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "privd attestation provisioning is not enabled")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node_id must be UUIDv7")
		return
	}
	var body privdCredentialRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	credential, err := s.privdAttestation.CreateCredential(r.Context(), privdattestation.CredentialRequest{
		NodeID: nodeID, IdentityID: actor.IdentityID, SessionID: actor.SessionID,
		TTL: time.Duration(body.TTLSeconds) * time.Second, RequestID: requestID(r), Reason: body.Reason,
	})
	if err != nil {
		s.writePrivdAttestationError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": credential.ID, "node_id": credential.NodeID, "credential": credential.Value,
		"controller_nonce_hex":          hex.EncodeToString(credential.ControllerNonce),
		"credential_context_sha256_hex": hex.EncodeToString(credential.CredentialContextSHA256),
		"expires_at":                    credential.ExpiresAt,
	})
}

func (s *Server) registerPrivdAttestationKey(w http.ResponseWriter, r *http.Request) {
	if s.privdAttestation == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "privd attestation provisioning is not enabled")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node_id must be UUIDv7")
		return
	}
	var body privdRegistrationRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	decode := func(value string) ([]byte, bool) {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		return decoded, decodeErr == nil
	}
	publicKey, publicOK := decode(body.PublicKey)
	nonce, nonceOK := decode(body.ControllerNonce)
	contextDigest, contextOK := decode(body.CredentialContextSHA256)
	signature, signatureOK := decode(body.Signature)
	if !publicOK || !nonceOK || !contextOK || !signatureOK {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "attestation registration encoding is invalid")
		return
	}
	keyID, err := s.privdAttestation.Register(r.Context(), privdattestation.RegistrationRequest{
		NodeID: nodeID, Credential: body.Credential, RequestID: requestID(r),
		Registration: &agentv1.PrivdAttestationRegistrationV1{
			Version: agentv1.PrivdReceiptVersion(body.Version), NodeId: nodeID[:],
			PrivdAttestationKeyId: body.KeyID, PublicKey: publicKey, ControllerNonce: nonce,
			CredentialContextSha256: contextDigest, Signature: signature,
		},
	})
	if err != nil {
		s.writePrivdAttestationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key_id": keyID, "state": "active"})
}

func (s *Server) revokePrivdAttestationKey(w http.ResponseWriter, r *http.Request) {
	if s.privdAttestation == nil {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "privd attestation provisioning is not enabled")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "node_id must be UUIDv7")
		return
	}
	var body privdRevokeRequest
	if !decodeStrictJSON(w, r, &body) {
		return
	}
	actor := principal(r)
	err = s.privdAttestation.Revoke(r.Context(), privdattestation.RevokeRequest{
		NodeID: nodeID, IdentityID: actor.IdentityID, SessionID: actor.SessionID,
		KeyID: body.KeyID, RequestID: requestID(r), Reason: body.Reason,
	})
	if err != nil {
		s.writePrivdAttestationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key_id": body.KeyID, "state": "revoked"})
}

func (s *Server) writePrivdAttestationError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	detail := "privd attestation request is invalid"
	if errors.Is(err, privdattestation.ErrCredential) {
		status, detail = http.StatusUnauthorized, "privd attestation credential is invalid, expired, replayed, or consumed"
	} else if errors.Is(err, privdattestation.ErrRotationLimit) {
		status, detail = http.StatusConflict, "privd attestation rotation overlap is full"
	} else if errors.Is(err, privdattestation.ErrKeyNotFound) {
		status, detail = http.StatusNotFound, "privd attestation key was not found"
	} else if !errors.Is(err, privdattestation.ErrInvalidRequest) {
		status, detail = http.StatusServiceUnavailable, "privd attestation storage is unavailable"
	}
	writeProblem(w, r, status, "https://ocservia.dev/problems/privd-attestation", "Privd attestation request failed", strings.TrimSpace(detail))
}
