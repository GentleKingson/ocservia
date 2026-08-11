package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/jackc/pgx/v5"
)

type createCertificateRequest struct {
	ExpectedVersion int64    `json:"expected_version"`
	CommonName      string   `json:"common_name"`
	DNSNames        []string `json:"dns_names,omitempty"`
	KeyBits         uint32   `json:"key_bits"`
	Reason          string   `json:"reason"`
}

type issueCertificateRequest struct {
	ApprovalID string `json:"approval_id"`
	Reason     string `json:"reason"`
}

type revokeCertificateRequest struct {
	ExpectedVersion    int64  `json:"expected_version"`
	CertificateVersion int64  `json:"certificate_version"`
	ApprovalID         string `json:"approval_id"`
	Reason             string `json:"reason"`
}

type createP12Request struct {
	ExpectedVersion    int64  `json:"expected_version"`
	CertificateVersion int64  `json:"certificate_version"`
	ApprovalID         string `json:"approval_id"`
	Reason             string `json:"reason"`
}

func (s *Server) createCertificate(w http.ResponseWriter, r *http.Request) {
	if s.certificates == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service unavailable", "certificate service is unavailable")
		return
	}
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var body createCertificateRequest
	if key == "" || !decodeStrict(w, r, &body) {
		if key == "" {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		}
		return
	}
	actor := principal(r)
	value, replayed, err := s.certificates.Create(r.Context(), certificates.CreateRequest{NodeID: nodeID, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: key, CommonName: body.CommonName, DNSNames: body.DNSNames, KeyBits: body.KeyBits, ActorID: actorID(r), ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, Reason: body.Reason, RequestID: requestID(r), Traceparent: requestTraceparent(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/certificates/"+value.ID.String())
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) listNodeCertificates(w http.ResponseWriter, r *http.Request) {
	nodeID, err := parseUUIDv7(r.PathValue("node_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "node_id must be UUIDv7")
		return
	}
	values, err := s.certificates.ListNode(r.Context(), nodeID)
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": values})
}

func (s *Server) issueCertificate(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(strings.TrimSuffix(r.PathValue("certificate_action"), ":issue"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "certificate_id must be UUIDv7")
		return
	}
	var body issueCertificateRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	approvalID, err := parseUUIDv7(body.ApprovalID)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	actor := principal(r)
	value, err := s.certificates.Issue(r.Context(), certificates.IssueRequest{CertificateID: id, ApprovalID: approvalID, ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, Reason: body.Reason, RequestID: requestID(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) revokeCertificate(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(strings.TrimSuffix(r.PathValue("certificate_action"), ":revoke"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "certificate_id must be UUIDv7")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var body revokeCertificateRequest
	if key == "" || !decodeStrict(w, r, &body) {
		if key == "" {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		}
		return
	}
	actor := principal(r)
	approvalID, parseErr := parseUUIDv7(body.ApprovalID)
	if parseErr != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	value, replayed, err := s.certificates.Revoke(r.Context(), certificates.RevokeRequest{CertificateID: id, ApprovalID: approvalID, CertificateVersion: body.CertificateVersion, ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: key, Reason: body.Reason, RequestID: requestID(r), Traceparent: requestTraceparent(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) createCertificateP12(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(strings.TrimSuffix(r.PathValue("certificate_action"), ":p12"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "certificate_id must be UUIDv7")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var body createP12Request
	if key == "" || !decodeStrict(w, r, &body) {
		if key == "" {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		}
		return
	}
	actor := principal(r)
	approvalID, parseErr := parseUUIDv7(body.ApprovalID)
	if parseErr != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "approval_id must be UUIDv7")
		return
	}
	value, replayed, err := s.certificates.CreateP12(r.Context(), certificates.P12Request{CertificateID: id, ApprovalID: approvalID, CertificateVersion: body.CertificateVersion, ActorIdentityID: actor.IdentityID, ActorSessionID: actor.SessionID, ExpectedVersion: body.ExpectedVersion, IdempotencyKey: key, Reason: body.Reason, RequestID: requestID(r), Traceparent: requestTraceparent(r)})
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(r.PathValue("artifact_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "artifact_id must be UUIDv7")
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-Artifact-Token"))
	if token == "" {
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/artifact-denied", "Artifact unavailable", "the artifact token is invalid, expired, or consumed")
		return
	}
	actor := principal(r)
	download, err := s.certificates.OpenArtifact(r.Context(), id, token, actor.IdentityID)
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	defer download.Reader.Close()
	data, readErr := io.ReadAll(io.LimitReader(download.Reader, download.Size+1))
	digest := sha256.Sum256(data)
	if readErr != nil || int64(len(data)) != download.Size || !bytes.Equal(digest[:], download.ExpectedSHA256) {
		_ = s.certificates.AbortArtifact(context.WithoutCancel(r.Context()), id, download.GrantID)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/artifact-unavailable", "Artifact unavailable", "the bounded artifact stream failed integrity verification")
		return
	}
	if err := s.certificates.CompleteArtifact(context.WithoutCancel(r.Context()), id, download.GrantID, download.Grant, digest[:], int64(len(data)), actor.IdentityID, actor.SessionID, requestID(r)); err != nil {
		writeCertificateError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", `attachment; filename="certificate.p12"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", download.Size))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) getCertificate(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDv7(r.PathValue("certificate_id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", "certificate_id must be UUIDv7")
		return
	}
	value, err := s.certificates.Get(r.Context(), id)
	if err != nil {
		writeCertificateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeCertificateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, certificates.ErrInvalid), errors.Is(err, operationstore.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/certificate-invalid", "Certificate request is invalid", "certificate subject, lifetime, or key parameters are invalid")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the idempotency key was used for different certificate content")
	case errors.Is(err, operationstore.ErrCapabilityMissing):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/capability-unavailable", "Capability is unavailable", "the node cannot generate certificate keys")
	case errors.Is(err, certificates.ErrNotReady):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/certificate-not-ready", "Certificate is not ready", "the CSR must be ready and the approval must match")
	case errors.Is(err, certificates.ErrSignerUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/signer-unavailable", "Certificate signer unavailable", "the external PKI signer is temporarily unavailable")
	case errors.Is(err, certificates.ErrArtifactDenied):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/artifact-denied", "Artifact unavailable", "the artifact token is invalid, expired, consumed, or already in use")
	case errors.Is(err, certificates.ErrArtifactCapacity):
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/artifact-capacity", "Artifact capacity is busy", "retry after another bounded artifact stream completes")
	case errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the certificate or node does not exist")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/certificate-unavailable", "Certificate service unavailable", "certificate processing is temporarily unavailable")
	}
}
