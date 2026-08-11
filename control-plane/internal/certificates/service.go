package certificates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

var (
	ErrInvalid           = errors.New("invalid certificate request")
	ErrNotReady          = errors.New("certificate is not ready")
	ErrSignerUnavailable = errors.New("certificate signer is unavailable")
	ErrArtifactDenied    = errors.New("artifact access denied")
	ErrArtifactCapacity  = errors.New("artifact download capacity is exhausted")
)

type SignRequest struct {
	CertificateID uuid.UUID
	CSRDER        []byte
}

type SignResult struct {
	CertificateChainPEM []byte
}

// Signer is an external PKI/HSM boundary. Implementations must make
// CertificateID idempotent; the controller never receives a CA private key.
type Signer interface {
	Sign(context.Context, SignRequest) (SignResult, error)
	Revoke(context.Context, RevokeSignerRequest) error
}

type SecretSealer interface {
	Seal(context.Context, uuid.UUID, agentv1.SealedSecretPurpose, []byte) (*agentv1.SealedSecretV1, error)
}

type ArtifactFetcher interface {
	FetchArtifact(context.Context, *agentv1.ArtifactGrantV1) (io.ReadCloser, error)
	ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) error
	ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64) (bool, error)
}

type RevokeSignerRequest struct {
	CertificateID uuid.UUID
	SerialNumber  string
	Reason        string
}

type Service struct {
	pool        *pgxpool.Pool
	operations  *operationstore.Service
	approvals   *approvals.Service
	signer      Signer
	sealer      SecretSealer
	artifacts   ArtifactFetcher
	grantSigner *commandauth.Signer
	now         func() time.Time
}

type CreateRequest struct {
	NodeID, ActorIdentityID, ActorSessionID uuid.UUID
	ExpectedVersion                         int64
	IdempotencyKey, CommonName              string
	DNSNames                                []string
	KeyBits                                 uint32
	ActorID, Reason, RequestID, Traceparent string
}

type Certificate struct {
	ID              uuid.UUID  `json:"id"`
	WorkspaceID     uuid.UUID  `json:"workspace_id"`
	NodeID          uuid.UUID  `json:"node_id"`
	OperationID     uuid.UUID  `json:"operation_id"`
	CommonName      string     `json:"common_name"`
	DNSNames        []string   `json:"dns_names"`
	KeyBits         uint32     `json:"key_bits"`
	State           string     `json:"state"`
	Version         int64      `json:"version"`
	PublicKeySHA256 []byte     `json:"public_key_sha256,omitempty"`
	SerialNumber    string     `json:"serial_number,omitempty"`
	NotBefore       *time.Time `json:"not_before,omitempty"`
	NotAfter        *time.Time `json:"not_after,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type IssueRequest struct {
	CertificateID, ApprovalID, ActorIdentityID, ActorSessionID uuid.UUID
	Reason, RequestID                                          string
}

type RevokeRequest struct {
	CertificateID, ApprovalID, ActorIdentityID, ActorSessionID uuid.UUID
	CertificateVersion                                         int64
	ExpectedVersion                                            int64
	IdempotencyKey, Reason, RequestID, Traceparent             string
}

type P12Request struct {
	CertificateID, ApprovalID, ArtifactRequestID, ActorIdentityID, ActorSessionID uuid.UUID
	CertificateVersion                                                            int64
	ExpectedVersion                                                               int64
	IdempotencyKey, Reason, RequestID, Traceparent                                string
}

type ArtifactGrant struct {
	ArtifactID    uuid.UUID                `json:"artifact_id"`
	Operation     operationstore.Operation `json:"operation"`
	DownloadToken string                   `json:"download_token,omitempty"`
	Password      string                   `json:"password,omitempty"`
	ExpiresAt     time.Time                `json:"expires_at"`
}

type ArtifactDownload struct {
	Reader         io.ReadCloser
	ExpectedSHA256 []byte
	Size           int64
	NodeID         uuid.UUID
	GrantID        uuid.UUID
	Grant          *agentv1.ArtifactGrantV1
}

func New(pool *pgxpool.Pool, operations *operationstore.Service) *Service {
	return &Service{pool: pool, operations: operations, approvals: approvals.New(pool), now: func() time.Time { return time.Now().UTC() }}
}

func NewWithSigner(pool *pgxpool.Pool, operations *operationstore.Service, signer Signer) *Service {
	service := New(pool, operations)
	service.signer = signer
	return service
}

func NewWithDependencies(pool *pgxpool.Pool, operations *operationstore.Service, signer Signer, sealer SecretSealer, artifacts ArtifactFetcher, grantSigner *commandauth.Signer) *Service {
	service := NewWithSigner(pool, operations, signer)
	service.sealer, service.artifacts, service.grantSigner = sealer, artifacts, grantSigner
	return service
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Certificate, bool, error) {
	if request.NodeID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.CommonName) == "" || strings.TrimSpace(request.Reason) == "" {
		return Certificate{}, false, ErrInvalid
	}
	certificateID, err := uuid.NewV7()
	if err != nil {
		return Certificate{}, false, err
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{
		NodeID: request.NodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey,
		Kind: operationstore.CertificateCSR, CertificateID: certificateID, CommonName: request.CommonName,
		DNSNames: request.DNSNames, KeyBits: request.KeyBits, ActorID: request.ActorID,
		ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID,
		Action: "certificate.issue", Reason: request.Reason, RequestID: request.RequestID,
		Traceparent: request.Traceparent, TTL: 15 * time.Minute,
	})
	if err != nil {
		return Certificate{}, false, err
	}
	if replay {
		operationID, parseErr := uuid.Parse(op.ID)
		if parseErr != nil {
			return Certificate{}, false, parseErr
		}
		return s.GetByOperation(ctx, operationID, true)
	}
	certificate, err := s.Get(ctx, certificateID)
	return certificate, false, err
}

func (s *Service) ApprovalBinding(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, requestHash []byte, summary json.RawMessage, err error) {
	var csrDER []byte
	var commonName string
	var dnsNames []string
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,common_name,dns_names FROM certificates WHERE id=$1 AND state='csr_ready'`, id).Scan(&workspaceID, &nodeID, &csrDER, &commonName, &dnsNames)
	if err != nil {
		return
	}
	digest := sha256.Sum256(append(append([]byte(id.String()+"\x00"), csrDER...), []byte("\x00certificate.issue")...))
	requestHash = digest[:]
	summary, err = json.Marshal(map[string]any{"certificate_id": id, "node_id": nodeID, "common_name": commonName, "dns_names": dnsNames, "csr_sha256": hex.EncodeToString(sha256Bytes(csrDER))})
	return
}

func (s *Service) ActionApprovalBinding(ctx context.Context, action string, certificateID uuid.UUID, certificateVersion int64, purpose string, artifactRequestID uuid.UUID) (workspaceID, nodeID uuid.UUID, requestHash []byte, summary json.RawMessage, err error) {
	var currentVersion int64
	var serial string
	var chain []byte
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,version,COALESCE(serial_number,''),COALESCE(certificate_chain_pem,''::bytea) FROM certificates WHERE id=$1`, certificateID).Scan(&workspaceID, &nodeID, &currentVersion, &serial, &chain)
	if err != nil {
		return
	}
	if certificateVersion < 1 || currentVersion != certificateVersion {
		err = ErrNotReady
		return
	}
	if action == "certificate.revoke" {
		chain = nil
	}
	requestHash, summary, err = certificateActionBinding(action, certificateID, nodeID, currentVersion, purpose, artifactRequestID, serial, chain)
	return
}

func certificateActionBinding(action string, certificateID, nodeID uuid.UUID, version int64, purpose string, artifactRequestID uuid.UUID, serial string, chain []byte) ([]byte, json.RawMessage, error) {
	if action != "certificate.revoke" && action != "certificate.private_key.export" {
		return nil, nil, ErrInvalid
	}
	if action == "certificate.private_key.export" && (purpose != "certificate_p12" || artifactRequestID == uuid.Nil) {
		return nil, nil, ErrInvalid
	}
	if action == "certificate.revoke" && (artifactRequestID != uuid.Nil || strings.TrimSpace(purpose) == "" || len(purpose) > 128) {
		return nil, nil, ErrInvalid
	}
	chainHash := sha256.Sum256(chain)
	type content struct {
		Action             string     `json:"action"`
		CertificateID      uuid.UUID  `json:"certificate_id"`
		CertificateVersion int64      `json:"certificate_version"`
		NodeID             uuid.UUID  `json:"node_id"`
		Purpose            string     `json:"purpose,omitempty"`
		ArtifactRequestID  *uuid.UUID `json:"artifact_request_id,omitempty"`
		SerialNumber       string     `json:"serial_number,omitempty"`
		CertificateSHA256  string     `json:"certificate_sha256,omitempty"`
	}
	var artifact *uuid.UUID
	if artifactRequestID != uuid.Nil {
		artifact = &artifactRequestID
	}
	value := content{Action: action, CertificateID: certificateID, CertificateVersion: version, NodeID: nodeID, Purpose: purpose, ArtifactRequestID: artifact, SerialNumber: serial}
	if len(chain) > 0 {
		value.CertificateSHA256 = hex.EncodeToString(chainHash[:])
	}
	summary, _ := json.Marshal(value)
	digest := sha256.Sum256(append([]byte("ocservia/certificate-approval/v1\x00"), summary...))
	return digest[:], summary, nil
}

func (s *Service) Issue(ctx context.Context, request IssueRequest) (Certificate, error) {
	if request.CertificateID == uuid.Nil || request.ApprovalID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return Certificate{}, ErrInvalid
	}
	if s.signer == nil {
		return Certificate{}, ErrSignerUnavailable
	}
	workspaceID, nodeID, csrDER, requestHash, err := s.prepareIssue(ctx, request)
	if err != nil {
		return Certificate{}, err
	}
	result, err := s.signer.Sign(ctx, SignRequest{CertificateID: request.CertificateID, CSRDER: append([]byte(nil), csrDER...)})
	if err != nil {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE certificates SET state='signer_unavailable',version=version+1,updated_at=$2 WHERE id=$1 AND state='signing' AND issue_approval_id=$3 AND issue_request_hash=$4`, request.CertificateID, s.now(), request.ApprovalID, requestHash)
		return Certificate{}, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	leaf, err := validateSignedCertificate(csrDER, result.CertificateChainPEM, s.now())
	if err != nil {
		return Certificate{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Certificate{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now := s.now()
	resultTag, err := tx.Exec(ctx, `UPDATE certificates SET state='issued',version=version+1,certificate_chain_pem=$2,serial_number=$3,not_before=$4,not_after=$5,updated_at=$6 WHERE id=$1 AND state IN ('signing','signer_unavailable') AND issue_approval_id=$7 AND issue_request_hash=$8`, request.CertificateID, result.CertificateChainPEM, leaf.SerialNumber.String(), leaf.NotBefore.UTC(), leaf.NotAfter.UTC(), now, request.ApprovalID, requestHash)
	if err != nil {
		return Certificate{}, err
	}
	if resultTag.RowsAffected() != 1 {
		return Certificate{}, ErrNotReady
	}
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"serial_number":%q,"not_after":%q}`, nodeID, leaf.SerialNumber.String(), leaf.NotAfter.UTC().Format(time.RFC3339))), At: now}); err != nil {
		return Certificate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Certificate{}, err
	}
	return s.Get(ctx, request.CertificateID)
}

func (s *Service) prepareIssue(ctx context.Context, request IssueRequest) (workspaceID, nodeID uuid.UUID, csrDER, requestHash []byte, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state string
	var storedApproval, storedActor *uuid.UUID
	var storedHash []byte
	err = tx.QueryRow(ctx, `SELECT workspace_id,node_id,csr_der,state,issue_approval_id,issue_request_hash,issue_actor_identity_id FROM certificates WHERE id=$1 FOR UPDATE`, request.CertificateID).Scan(&workspaceID, &nodeID, &csrDER, &state, &storedApproval, &storedHash, &storedActor)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, nil, nil, ErrNotReady
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, nil, err
	}
	digest := sha256.Sum256(append(append([]byte(request.CertificateID.String()+"\x00"), csrDER...), []byte("\x00certificate.issue")...))
	requestHash = digest[:]
	switch state {
	case "csr_ready":
		if err := approvals.ConsumeBound(ctx, tx, request.ApprovalID, workspaceID, request.ActorIdentityID, "certificate.issue", "certificate", request.CertificateID, requestHash); err != nil {
			return uuid.Nil, uuid.Nil, nil, nil, err
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,issue_approval_id=$2,issue_request_hash=$3,issue_actor_identity_id=$4,updated_at=$5 WHERE id=$1`, request.CertificateID, request.ApprovalID, requestHash, request.ActorIdentityID, now); err != nil {
			return uuid.Nil, uuid.Nil, nil, nil, err
		}
		if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, NodeID: &nodeID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID, Result: "intent", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"csr_sha256":%q,"state":"signing"}`, hex.EncodeToString(sha256Bytes(csrDER)))), At: now}); err != nil {
			return uuid.Nil, uuid.Nil, nil, nil, err
		}
	case "signing", "signer_unavailable":
		if storedApproval == nil || storedActor == nil || *storedApproval != request.ApprovalID || *storedActor != request.ActorIdentityID || !bytes.Equal(storedHash, requestHash) {
			return uuid.Nil, uuid.Nil, nil, nil, ErrNotReady
		}
		if state == "signer_unavailable" {
			if _, err := tx.Exec(ctx, `UPDATE certificates SET state='signing',version=version+1,updated_at=$2 WHERE id=$1`, request.CertificateID, s.now()); err != nil {
				return uuid.Nil, uuid.Nil, nil, nil, err
			}
		}
	default:
		return uuid.Nil, uuid.Nil, nil, nil, ErrNotReady
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, uuid.Nil, nil, nil, err
	}
	return workspaceID, nodeID, csrDER, requestHash, nil
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (operationstore.Operation, bool, error) {
	if request.CertificateID == uuid.Nil || request.ApprovalID == uuid.Nil || request.CertificateVersion < 1 || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return operationstore.Operation{}, false, ErrInvalid
	}
	if s.signer == nil {
		return operationstore.Operation{}, false, ErrSignerUnavailable
	}
	var workspaceID, nodeID uuid.UUID
	var serialNumber, state string
	var certificateVersion int64
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,COALESCE(serial_number,''),state,version FROM certificates WHERE id=$1`, request.CertificateID).Scan(&workspaceID, &nodeID, &serialNumber, &state, &certificateVersion); err != nil {
		return operationstore.Operation{}, false, err
	}
	var existingRequest bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$1 AND idempotency_key=$2)`, workspaceID, request.IdempotencyKey).Scan(&existingRequest); err != nil {
		return operationstore.Operation{}, false, err
	}
	if certificateVersion != request.CertificateVersion && !existingRequest {
		return operationstore.Operation{}, false, ErrNotReady
	}
	requestHash, _, err := certificateActionBinding("certificate.revoke", request.CertificateID, nodeID, request.CertificateVersion, request.Reason, uuid.Nil, serialNumber, nil)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if state == "revoked" {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$1 AND idempotency_key=$2)`, workspaceID, request.IdempotencyKey).Scan(&exists); err != nil {
			return operationstore.Operation{}, false, err
		}
		if !exists {
			return operationstore.Operation{}, false, ErrNotReady
		}
	}
	if state != "issued" && state != "expiring" && state != "expired" && state != "revoking" && state != "revocation_unknown" && state != "revoked" || serialNumber == "" {
		return operationstore.Operation{}, false, ErrNotReady
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey, Kind: operationstore.CertificateRevoke, CertificateID: request.CertificateID, CertificateVersion: uint64(request.CertificateVersion), RevocationReason: request.Reason, ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID, ApprovalID: request.ApprovalID, ApprovalRequestHash: requestHash, Action: "certificate.revoke", Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, TTL: 15 * time.Minute, HoldDispatch: true})
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	if replay && op.State == "expired" {
		return op, true, ErrNotReady
	}
	if replay && state == "revoked" {
		return op, true, nil
	}
	tx, txErr := s.pool.Begin(ctx)
	if txErr != nil {
		return op, replay, txErr
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, txErr = tx.Exec(ctx, `UPDATE certificates SET state='revoking',version=version+1,updated_at=$2 WHERE id=$1 AND state IN ('issued','expiring','expired','revocation_unknown')`, request.CertificateID, s.now()); txErr != nil {
		return op, replay, txErr
	}
	if _, txErr = tx.Exec(ctx, `UPDATE artifact_operations SET state='revoked',lease_until=NULL,active_grant_expires_at=now(),updated_at=now() WHERE certificate_id=$1 AND state IN ('pending','ready','leased','consuming')`, request.CertificateID); txErr != nil {
		return op, replay, txErr
	}
	if txErr = tx.Commit(ctx); txErr != nil {
		return op, replay, txErr
	}
	if err := s.signer.Revoke(ctx, RevokeSignerRequest{CertificateID: request.CertificateID, SerialNumber: serialNumber, Reason: request.Reason}); err != nil {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE certificates SET state='revocation_unknown',version=version+1,updated_at=$2 WHERE id=$1 AND state='revoking'`, request.CertificateID, s.now())
		return op, replay, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	operationID, parseErr := uuid.Parse(op.ID)
	if parseErr != nil {
		return op, replay, parseErr
	}
	if err := s.releaseOperation(ctx, operationID); err != nil {
		return op, replay, err
	}
	return op, replay, nil
}

func (s *Service) releaseOperation(ctx context.Context, operationID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var outboxID uuid.UUID
	err = tx.QueryRow(ctx, `UPDATE outbox_events SET available_at=now() WHERE command_id=(SELECT command_id FROM operations WHERE id=$1) AND published_at IS NULL RETURNING id`, operationID).Scan(&outboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, outboxID.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) CreateP12(ctx context.Context, request P12Request) (ArtifactGrant, bool, error) {
	if request.ArtifactRequestID == uuid.Nil && request.ApprovalID != uuid.Nil {
		approval, getErr := s.approvals.Get(ctx, request.ApprovalID)
		if getErr != nil {
			return ArtifactGrant{}, false, ErrNotReady
		}
		var content struct {
			ArtifactRequestID  uuid.UUID `json:"artifact_request_id"`
			CertificateVersion int64     `json:"certificate_version"`
		}
		if approval.Action != "certificate.private_key.export" || approval.ResourceID != request.CertificateID || json.Unmarshal(approval.CertificateSummary, &content) != nil || content.CertificateVersion != request.CertificateVersion {
			return ArtifactGrant{}, false, ErrNotReady
		}
		request.ArtifactRequestID = content.ArtifactRequestID
	}
	if request.CertificateID == uuid.Nil || request.ApprovalID == uuid.Nil || request.ArtifactRequestID == uuid.Nil || request.CertificateVersion < 1 || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return ArtifactGrant{}, false, ErrInvalid
	}
	if s.sealer == nil {
		return ArtifactGrant{}, false, ErrSignerUnavailable
	}
	var workspaceID, nodeID uuid.UUID
	var chain []byte
	var serialNumber string
	var certificateVersion int64
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id,node_id,certificate_chain_pem,serial_number,version FROM certificates WHERE id=$1 AND state IN ('issued','expiring') AND not_after>now()`, request.CertificateID).Scan(&workspaceID, &nodeID, &chain, &serialNumber, &certificateVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ArtifactGrant{}, false, ErrNotReady
		}
		return ArtifactGrant{}, false, err
	}
	if certificateVersion != request.CertificateVersion {
		return ArtifactGrant{}, false, ErrNotReady
	}
	intent, _ := json.Marshal(map[string]any{"certificate_id": request.CertificateID, "certificate_version": request.CertificateVersion, "artifact_request_id": request.ArtifactRequestID, "approval_id": request.ApprovalID, "node_id": nodeID, "expected_version": request.ExpectedVersion, "actor_identity_id": request.ActorIdentityID, "actor_session_id": request.ActorSessionID, "reason": request.Reason})
	intentHash := sha256.Sum256(intent)
	var existingArtifactID, existingOperationID uuid.UUID
	var existingExpires time.Time
	var sameIntent bool
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=$3 FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspaceID, request.IdempotencyKey, intentHash[:]).Scan(&existingArtifactID, &existingOperationID, &existingExpires, &sameIntent)
	if err == nil {
		if !sameIntent {
			return ArtifactGrant{}, false, operationstore.ErrIdempotencyConflict
		}
		op, getErr := s.operations.Get(ctx, existingOperationID)
		if getErr != nil {
			return ArtifactGrant{}, false, getErr
		}
		return ArtifactGrant{ArtifactID: existingArtifactID, Operation: op, ExpiresAt: existingExpires}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ArtifactGrant{}, false, err
	}
	passwordBytes, tokenBytes := make([]byte, 24), make([]byte, 32)
	if _, err := rand.Read(passwordBytes); err != nil {
		return ArtifactGrant{}, false, err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return ArtifactGrant{}, false, err
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes)
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	for index := range passwordBytes {
		passwordBytes[index] = 0
	}
	sealed, err := s.sealer.Seal(ctx, nodeID, agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, []byte(password))
	if err != nil {
		return ArtifactGrant{}, false, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	var registeredKey bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sealing_keys WHERE node_id=$1 AND purpose=2 AND version=$2 AND key_id=$3)`, nodeID, sealed.GetVersion(), sealed.GetKeyId()).Scan(&registeredKey); err != nil {
		return ArtifactGrant{}, false, err
	}
	if !registeredKey {
		return ArtifactGrant{}, false, ErrSignerUnavailable
	}
	artifactID := request.ArtifactRequestID
	expiresAt := s.now().Add(10 * time.Minute)
	tokenHash := sha256.Sum256([]byte(token))
	approvalHash, _, err := certificateActionBinding("certificate.private_key.export", request.CertificateID, nodeID, certificateVersion, "certificate_p12", artifactID, serialNumber, chain)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey, Kind: operationstore.CertificateP12, CertificateID: request.CertificateID, CertificateVersion: uint64(certificateVersion), CertificateChain: chain, SealedPassword: sealed, ArtifactID: artifactID, ArtifactMetadata: &operationstore.ArtifactMetadata{TokenSHA256: tokenHash[:], RequestHash: intentHash[:], ExpiresAt: expiresAt}, ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID, ApprovalID: request.ApprovalID, ApprovalRequestHash: approvalHash, Action: "certificate.private_key.export", Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, TTL: 15 * time.Minute})
	if err != nil {
		if errors.Is(err, operationstore.ErrIdempotencyConflict) {
			var id, operationID uuid.UUID
			var expiration time.Time
			var same bool
			lookupErr := s.pool.QueryRow(ctx, `SELECT a.id,a.operation_id,a.expires_at,a.request_hash=$3 FROM operations o JOIN artifact_operations a ON a.operation_id=o.id WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspaceID, request.IdempotencyKey, intentHash[:]).Scan(&id, &operationID, &expiration, &same)
			if lookupErr == nil && same {
				existingOperation, getErr := s.operations.Get(ctx, operationID)
				if getErr != nil {
					return ArtifactGrant{}, false, getErr
				}
				return ArtifactGrant{ArtifactID: id, Operation: existingOperation, ExpiresAt: expiration}, true, nil
			}
		}
		return ArtifactGrant{}, false, err
	}
	if replay {
		var existingID uuid.UUID
		var existingExpiry time.Time
		if err := s.pool.QueryRow(ctx, `SELECT id,expires_at FROM artifact_operations WHERE operation_id=$1`, op.ID).Scan(&existingID, &existingExpiry); err != nil {
			return ArtifactGrant{}, false, err
		}
		return ArtifactGrant{ArtifactID: existingID, Operation: op, ExpiresAt: existingExpiry}, true, nil
	}
	return ArtifactGrant{ArtifactID: artifactID, Operation: op, DownloadToken: token, Password: password, ExpiresAt: expiresAt}, false, nil
}

func (s *Service) ArtifactResource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM artifact_operations WHERE id=$1`, id).Scan(&workspaceID, &nodeID)
	return
}

func (s *Service) OpenArtifact(ctx context.Context, id uuid.UUID, token string, subject uuid.UUID) (ArtifactDownload, error) {
	if id == uuid.Nil || subject == uuid.Nil || len(token) != 43 || s.artifacts == nil || s.grantSigner == nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	tokenHash := sha256.Sum256([]byte(token))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ArtifactDownload{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(6820260817)`); err != nil {
		return ArtifactDownload{}, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifact_operations WHERE state='leased' AND lease_until>now()`).Scan(&active); err != nil {
		return ArtifactDownload{}, err
	}
	if active >= 4 {
		return ArtifactDownload{}, ErrArtifactCapacity
	}
	var nodeID, certificateID, operationID uuid.UUID
	var expected []byte
	var size int64
	var certificateVersion uint64
	var artifactExpires, certificateExpires time.Time
	var requesterID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT a.node_id,a.certificate_id,a.operation_id,a.certificate_version,a.content_sha256,a.content_size,a.expires_at,c.not_after,r.requester_id FROM artifact_operations a JOIN certificates c ON c.id=a.certificate_id JOIN approval_requests r ON r.id=a.approval_id WHERE a.id=$1 AND a.token_sha256=$2 AND a.expires_at>now() AND c.not_after>now() AND c.state IN ('issued','expiring') AND (a.state='ready' OR (a.state='leased' AND a.lease_until<now())) FOR UPDATE OF a`, id, tokenHash[:]).Scan(&nodeID, &certificateID, &operationID, &certificateVersion, &expected, &size, &artifactExpires, &certificateExpires, &requesterID)
	if err != nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	if requesterID != subject {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	grantID := uuid.Must(uuid.NewV7())
	issuedAt := s.now().UTC()
	grantExpires := issuedAt.Add(time.Minute)
	if artifactExpires.Before(grantExpires) {
		grantExpires = artifactExpires
	}
	if certificateExpires.Before(grantExpires) {
		grantExpires = certificateExpires
	}
	grant, err := s.grantSigner.IssueArtifactGrant(nodeID, id, certificateID, certificateVersion, operationID, requesterID.String(), uint64(size), grantID, issuedAt, grantExpires)
	if err != nil {
		return ArtifactDownload{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='leased',lease_until=$2,active_grant_id=$3,active_grant_subject=$4,active_grant_expires_at=$2,updated_at=now() WHERE id=$1`, id, grantExpires, grantID, subject.String()); err != nil {
		return ArtifactDownload{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArtifactDownload{}, err
	}
	reader, err := s.artifacts.FetchArtifact(ctx, grant)
	if err != nil {
		_ = s.AbortArtifact(context.WithoutCancel(ctx), id, grantID)
		return ArtifactDownload{}, err
	}
	return ArtifactDownload{Reader: reader, ExpectedSHA256: expected, Size: size, NodeID: nodeID, GrantID: grantID, Grant: grant}, nil
}

func (s *Service) CompleteArtifact(ctx context.Context, id, grantID uuid.UUID, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, actorID, sessionID uuid.UUID, requestID string) error {
	if grant == nil || actorID == uuid.Nil || sessionID == uuid.Nil || requestID == "" || len(requestID) > 128 || grant.GetAuthorizedSubject() != actorID.String() || !bytes.Equal(grant.GetArtifactId(), id[:]) || !bytes.Equal(grant.GetGrantId(), grantID[:]) || len(digest) != sha256.Size || size < 1 || uint64(size) != grant.GetMaxBytes() {
		return ErrArtifactDenied
	}
	grantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(grant)
	if err != nil || len(grantBytes) == 0 || len(grantBytes) > 4096 {
		return ErrArtifactDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	command, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='consuming',consume_grant=$6,consume_sha256=$3,consume_size=$4,consume_actor_id=$7,consume_session_id=$8,consume_request_id=$9,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND active_grant_subject=$5 AND state='leased' AND content_sha256=$3 AND content_size=$4 AND expires_at>now()`, id, grantID, digest, size, actorID.String(), grantBytes, actorID, sessionID, requestID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrArtifactDenied
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Persist the exact signed evidence before root consumes and deletes bytes.
	// Recovery can later confirm this same root record without authorizing a new
	// mutation after the grant expires.
	if err := s.artifacts.ConsumeArtifact(ctx, grant, digest, size); err != nil {
		return err
	}
	result, err := s.finalizeArtifactConsumption(ctx, id, grantID)
	if err != nil {
		return err
	}
	switch result {
	case artifactConsumptionFinalized, artifactConsumptionAlreadyFinalized:
		return nil
	default:
		return ErrArtifactDenied
	}
}

type artifactConsumptionFinalization uint8

const (
	artifactConsumptionFinalized artifactConsumptionFinalization = iota
	artifactConsumptionAlreadyFinalized
	artifactConsumptionRevoked
	artifactConsumptionExpired
)

func (s *Service) finalizeArtifactConsumption(ctx context.Context, id, grantID uuid.UUID) (artifactConsumptionFinalization, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var workspaceID, nodeID, certificateID, actorID, sessionID uuid.UUID
	var digest []byte
	var size int64
	var requestID string
	err = tx.QueryRow(ctx, `UPDATE artifact_operations SET state='consumed',consumed_at=now(),lease_until=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming' RETURNING workspace_id,node_id,certificate_id,consume_actor_id,consume_session_id,consume_request_id,consume_sha256,consume_size`, id, grantID).Scan(&workspaceID, &nodeID, &certificateID, &actorID, &sessionID, &requestID, &digest, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		var state string
		if queryErr := tx.QueryRow(ctx, `SELECT state FROM artifact_operations WHERE id=$1 AND active_grant_id=$2`, id, grantID).Scan(&state); queryErr != nil {
			return 0, ErrArtifactDenied
		}
		switch state {
		case "consumed":
			return artifactConsumptionAlreadyFinalized, nil
		case "revoked":
			return artifactConsumptionRevoked, nil
		case "expired":
			return artifactConsumptionExpired, nil
		default:
			return 0, ErrArtifactDenied
		}
	}
	if err != nil {
		return 0, err
	}
	now := s.now()
	if err := audit.AppendChain(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: actorID.String(), SessionID: &sessionID, Action: "certificate.p12.download", ResourceType: "artifact", ResourceID: id, NodeID: &nodeID, RequestID: requestID, Result: "succeeded", AfterSummary: json.RawMessage(fmt.Sprintf(`{"certificate_id":%q,"sha256":%q,"size":%d}`, certificateID, hex.EncodeToString(digest), size)), At: now}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return artifactConsumptionFinalized, nil
}

func (s *Service) reconcileConsumingArtifacts(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id,active_grant_id,consume_grant,consume_sha256,consume_size,consume_actor_id,expires_at FROM artifact_operations WHERE state='consuming' ORDER BY updated_at,id LIMIT 20`)
	if err != nil {
		return err
	}
	type pending struct {
		id, grantID uuid.UUID
		grant       *agentv1.ArtifactGrantV1
		digest      []byte
		size        int64
		actorID     uuid.UUID
		expiresAt   time.Time
	}
	values := make([]pending, 0, 20)
	for rows.Next() {
		var value pending
		var grantBytes []byte
		value.grant = &agentv1.ArtifactGrantV1{}
		if err := rows.Scan(&value.id, &value.grantID, &grantBytes, &value.digest, &value.size, &value.actorID, &value.expiresAt); err != nil {
			rows.Close()
			return err
		}
		if err := proto.Unmarshal(grantBytes, value.grant); err != nil || !bytes.Equal(value.grant.GetArtifactId(), value.id[:]) || !bytes.Equal(value.grant.GetGrantId(), value.grantID[:]) || value.grant.GetAuthorizedSubject() != value.actorID.String() || value.grant.GetMaxBytes() != uint64(value.size) {
			rows.Close()
			return ErrArtifactDenied
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, value := range values {
		consumed, confirmErr := s.artifacts.ConfirmArtifactConsumed(ctx, value.grant, value.digest, value.size)
		if confirmErr != nil {
			// Reconciliation is item-scoped. A disconnected node or restarting
			// Agent must not turn one durable artifact into a global scheduler
			// failure; exact evidence remains in consuming for the next pass.
			continue
		}
		grantExpires := value.grant.GetExpiresAt()
		if grantExpires == nil {
			return ErrArtifactDenied
		}
		if !consumed && grantExpires.AsTime().After(s.now()) {
			if err := s.artifacts.ConsumeArtifact(ctx, value.grant, value.digest, value.size); err != nil {
				continue
			}
			consumed = true
		}
		if consumed {
			// Revocation and expiry may win the terminal transition after root
			// confirmation. They are benign for reconciliation, but the explicit
			// result prevents an in-flight HTTP request from treating them as a
			// successful delivery.
			if _, err := s.finalizeArtifactConsumption(ctx, value.id, value.grantID); err != nil {
				return err
			}
		} else if !value.expiresAt.After(s.now()) {
			if _, err := s.pool.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming'`, value.id, value.grantID); err != nil {
				return err
			}
		} else if _, err := s.pool.Exec(ctx, `UPDATE artifact_operations SET state='ready',lease_until=NULL,active_grant_id=NULL,active_grant_subject=NULL,active_grant_expires_at=NULL,consume_grant=NULL,consume_sha256=NULL,consume_size=NULL,consume_actor_id=NULL,consume_session_id=NULL,consume_request_id=NULL,updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='consuming'`, value.id, value.grantID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) AbortArtifact(ctx context.Context, id, grantID uuid.UUID) error {
	// Preserve the durable lease until its bounded expiry. A transport failure
	// must not make a second concurrently valid grant immediately issuable.
	_, err := s.pool.Exec(ctx, `UPDATE artifact_operations SET updated_at=now() WHERE id=$1 AND active_grant_id=$2 AND state='leased'`, id, grantID)
	return err
}

func (s *Service) Maintain(ctx context.Context) error {
	if err := s.reconcileConsumingArtifacts(ctx); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	type expiryAlert struct {
		id          uuid.UUID
		workspaceID uuid.UUID
		nodeID      uuid.UUID
		notAfter    time.Time
		kind        string
	}
	alerts := make([]expiryAlert, 0, 200)
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT id FROM certificates WHERE state IN ('issued','expiring') AND not_after<=now()
		ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED
	) UPDATE certificates c SET state='expired',version=version+1,updated_at=now() FROM due WHERE c.id=due.id RETURNING c.id,c.workspace_id,c.node_id,c.not_after`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var alert expiryAlert
		if err := rows.Scan(&alert.id, &alert.workspaceID, &alert.nodeID, &alert.notAfter); err != nil {
			rows.Close()
			return err
		}
		alert.kind = "certificate.expired"
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `UPDATE artifact_operations a SET state='expired',lease_until=NULL,updated_at=now() FROM certificates c WHERE c.id=a.certificate_id AND c.state='expired' AND a.state IN ('pending','ready','leased','consuming')`); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `WITH due AS (
		SELECT id FROM certificates WHERE state='issued' AND not_after>now() AND not_after<=now()+interval '30 days'
		ORDER BY not_after,id LIMIT 100 FOR UPDATE SKIP LOCKED
	) UPDATE certificates c SET state='expiring',version=version+1,updated_at=now() FROM due WHERE c.id=due.id RETURNING c.id,c.workspace_id,c.node_id,c.not_after`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var alert expiryAlert
		if err := rows.Scan(&alert.id, &alert.workspaceID, &alert.nodeID, &alert.notAfter); err != nil {
			rows.Close()
			return err
		}
		alert.kind = "certificate.expiring"
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, alert := range alerts {
		if _, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,node_id,resource_type,resource_id,created_at) VALUES($1,$2,'high',$3,$4,'certificate',$5,$6)`, uuid.Must(uuid.NewV7()), alert.workspaceID, alert.kind, alert.nodeID, alert.id, s.now()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE artifact_operations SET state='expired',lease_until=NULL,updated_at=now() WHERE state IN ('pending','ready','leased') AND expires_at<=now()`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Certificate, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE id=$1`, id))
}

func (s *Service) ListNode(ctx context.Context, nodeID uuid.UUID) ([]Certificate, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE node_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Certificate, 0)
	for rows.Next() {
		value, scanErr := scan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Service) GetByOperation(ctx context.Context, operationID uuid.UUID, replay bool) (Certificate, bool, error) {
	certificate, err := scan(s.pool.QueryRow(ctx, `SELECT id,workspace_id,node_id,operation_id,common_name,dns_names,key_bits,state,version,public_key_sha256,COALESCE(serial_number,''),not_before,not_after,revoked_at,created_at,updated_at FROM certificates WHERE operation_id=$1`, operationID))
	return certificate, replay, err
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,node_id FROM certificates WHERE id=$1`, id).Scan(&workspaceID, &nodeID)
	return
}

func scan(row pgx.Row) (Certificate, error) {
	var certificate Certificate
	var keyBits int32
	err := row.Scan(&certificate.ID, &certificate.WorkspaceID, &certificate.NodeID, &certificate.OperationID, &certificate.CommonName, &certificate.DNSNames, &keyBits, &certificate.State, &certificate.Version, &certificate.PublicKeySHA256, &certificate.SerialNumber, &certificate.NotBefore, &certificate.NotAfter, &certificate.RevokedAt, &certificate.CreatedAt, &certificate.UpdatedAt)
	if err != nil {
		return Certificate{}, err
	}
	certificate.KeyBits = uint32(keyBits)
	return certificate, nil
}

func validateSignedCertificate(csrDER, chainPEM []byte, now time.Time) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil, ErrInvalid
	}
	if len(chainPEM) > 256*1024 {
		return nil, ErrInvalid
	}
	remaining := chainPEM
	chain := make([]*x509.Certificate, 0, 4)
	for len(bytes.TrimSpace(remaining)) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(chain) >= 8 {
			return nil, ErrInvalid
		}
		certificate, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return nil, ErrInvalid
		}
		chain = append(chain, certificate)
		remaining = rest
	}
	if len(chain) < 2 {
		return nil, ErrInvalid
	}
	leaf := chain[0]
	root := chain[len(chain)-1]
	if !root.IsCA || root.CheckSignature(root.SignatureAlgorithm, root.RawTBSCertificate, root.Signature) != nil {
		return nil, ErrInvalid
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, intermediate := range chain[1 : len(chain)-1] {
		if !intermediate.IsCA {
			return nil, ErrInvalid
		}
		intermediates.AddCert(intermediate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, ErrInvalid
	}
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || now.Before(leaf.NotBefore.Add(-5*time.Minute)) || !now.Before(leaf.NotAfter) || !bytes.Equal(csr.RawSubject, leaf.RawSubject) {
		return nil, ErrInvalid
	}
	if len(leaf.EmailAddresses) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 {
		return nil, ErrInvalid
	}
	csrNames, leafNames := slices.Clone(csr.DNSNames), slices.Clone(leaf.DNSNames)
	sort.Strings(csrNames)
	sort.Strings(leafNames)
	if !slices.Equal(csrNames, leafNames) {
		return nil, ErrInvalid
	}
	csrKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, ErrInvalid
	}
	leafKey, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !strings.EqualFold(hex.EncodeToString(csrKey), hex.EncodeToString(leafKey)) {
		return nil, ErrInvalid
	}
	return leaf, nil
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
