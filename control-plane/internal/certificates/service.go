package certificates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates/artifactstore"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
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
	FetchArtifact(context.Context, *agentv1.ArtifactGrantV1, *agentv1.FenceBindingV2) (io.ReadCloser, error)
	ConsumeArtifact(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) error
	ConfirmArtifactConsumed(context.Context, *agentv1.ArtifactGrantV1, []byte, int64, *agentv1.FenceBindingV2) (bool, error)
}

type RevokeSignerRequest struct {
	CertificateID uuid.UUID
	SerialNumber  string
	Reason        string
}

type Service struct {
	backend     database.Backend
	operations  *operationstore.Service
	approvals   *approvals.Service
	signer      Signer
	sealer      SecretSealer
	artifacts   ArtifactFetcher
	grantSigner *commandauth.Signer
	fences      ownersession.FencedExecutor
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
	ID              uuid.UUID        `json:"id"`
	WorkspaceID     uuid.UUID        `json:"workspace_id"`
	NodeID          uuid.UUID        `json:"node_id"`
	OperationID     uuid.UUID        `json:"operation_id"`
	CommonName      string           `json:"common_name"`
	DNSNames        json.RawMessage  `json:"dns_names"`
	KeyBits         uint32           `json:"key_bits"`
	State           string           `json:"state"`
	Version         int64            `json:"version"`
	PublicKeySHA256 []byte           `json:"public_key_sha256,omitempty"`
	SerialNumber    string           `json:"serial_number,omitempty"`
	NotBefore       *value.Timestamp `json:"not_before,omitempty"`
	NotAfter        *value.Timestamp `json:"not_after,omitempty"`
	RevokedAt       *value.Timestamp `json:"revoked_at,omitempty"`
	CreatedAt       value.Timestamp  `json:"created_at"`
	UpdatedAt       value.Timestamp  `json:"updated_at"`
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
	ExpiresAt     value.Timestamp          `json:"expires_at"`
}

type ArtifactDownload struct {
	Reader         io.ReadCloser
	ExpectedSHA256 []byte
	Size           int64
	NodeID         uuid.UUID
	GrantID        uuid.UUID
	Grant          *agentv1.ArtifactGrantV1
}

// NewBackend uses common certificate transactions. CSR/P12/revocation also
// require an operations service configured for the same backend.
func NewBackend(backend database.Backend, operations *operationstore.Service, signer Signer, sealer SecretSealer, artifacts ArtifactFetcher, grantSigner *commandauth.Signer) *Service {
	return &Service{backend: backend, operations: operations, approvals: approvals.NewBackend(backend), signer: signer, sealer: sealer, artifacts: artifacts, grantSigner: grantSigner, now: func() time.Time { return time.Now().UTC() }}
}

// NewArtifactDownloads constructs downloads, durable consumption recovery and
// expiry maintenance, and certificate reads without issuance dependencies.
func NewArtifactDownloads(backend database.Backend, artifacts ArtifactFetcher, signer *commandauth.Signer) *Service {
	return &Service{backend: backend, artifacts: artifacts, grantSigner: signer, now: func() time.Time { return time.Now().UTC() }}
}

// EnableOwnerFencing runs artifact mutations inside the connection owner's
// fencing interval. Artifact transfers are owner-fenced operations: a stale
// owner must neither fetch nor confirm artifacts, and no binding may outlive
// the term the ownership authority backed at mutation time.
func (s *Service) EnableOwnerFencing(fences ownersession.FencedExecutor) {
	s.fences = fences
}

// executeArtifactFenced runs one artifact transfer mutation inside the owner
// fencing interval. Fetches bind the artifact identity; consumes and
// confirmations bind the grant identity.
func (s *Service) executeArtifactFenced(ctx context.Context, nodeID, operationID uuid.UUID, action ownersession.FencedAction) error {
	if s.fences == nil {
		return action(ctx, nil, nil)
	}
	var fixedNode [16]byte
	copy(fixedNode[:], nodeID[:])
	var fixedOperation [16]byte
	copy(fixedOperation[:], operationID[:])
	return s.fences.ExecuteFenced(ctx, fixedNode, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, fixedOperation, ownersession.FencingCapability, action)
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
	var binding certificatestore.IssueState
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		binding, err = store.IssueBinding(ctx, id)
		return err
	})
	if err != nil {
		return
	}
	workspaceID, nodeID = binding.WorkspaceID, binding.NodeID
	requestHash = certificateIssueBinding(id, binding.Version, binding.CSR, binding.ReceiptDigest)
	summary, err = json.Marshal(map[string]any{"certificate_id": id, "node_id": nodeID, "common_name": binding.CommonName, "dns_names": json.RawMessage(binding.DNSNames.Bytes()), "csr_sha256": hex.EncodeToString(sha256Bytes(binding.CSR))})
	return
}

func (s *Service) ActionApprovalBinding(ctx context.Context, action string, certificateID uuid.UUID, certificateVersion int64, purpose string, artifactRequestID uuid.UUID) (workspaceID, nodeID uuid.UUID, requestHash []byte, summary json.RawMessage, err error) {
	var binding certificatestore.ActionBinding
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		binding, err = store.ActionBinding(ctx, certificateID)
		return err
	})
	if err != nil {
		return
	}
	workspaceID, nodeID = binding.WorkspaceID, binding.NodeID
	if certificateVersion < 1 || binding.Version != certificateVersion {
		err = ErrNotReady
		return
	}
	if action == "certificate.revoke" {
		binding.Chain = nil
	}
	requestHash, summary, err = certificateActionBinding(action, certificateID, nodeID, binding.Version, purpose, artifactRequestID, binding.Serial, binding.Chain)
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
	var approved certificatestore.IssueReceipt
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		approved, err = store.ApprovedReceipt(ctx, request.CertificateID, request.ApprovalID)
		return err
	})
	csrDigest := sha256.Sum256(csrDER)
	if err != nil || approved.NodeID != nodeID || !approved.Current || !approved.KeyActive || !bytes.Equal(approved.RequestHash, requestHash) || !bytes.Equal(approved.CSRDigest, csrDigest[:]) {
		return Certificate{}, ErrNotReady
	}
	result, err := s.signer.Sign(ctx, SignRequest{CertificateID: request.CertificateID, CSRDER: append([]byte(nil), csrDER...)})
	if err != nil {
		_ = database.Within(context.WithoutCancel(ctx), s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, storeErr := certificatestore.FromTransaction(tx)
			if storeErr != nil {
				return storeErr
			}
			return store.SignerUnavailable(context.WithoutCancel(ctx), request.CertificateID, request.ApprovalID, requestHash, s.now())
		})
		return Certificate{}, fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	leaf, err := validateSignedCertificate(csrDER, result.CertificateChainPEM, s.now())
	if err != nil {
		return Certificate{}, err
	}
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		now := s.now()
		current, err := store.LockReceipt(ctx, request.CertificateID)
		if err != nil || current.NodeID != nodeID || !current.Current || current.ReceiptKey != approved.ReceiptKey || !bytes.Equal(current.CSR, csrDER) || !bytes.Equal(current.ReceiptDigest, approved.ReceiptDigest) || !bytes.Equal(current.CSRDigest, approved.CSRDigest) || !bytes.Equal(current.SubjectDigest, approved.SubjectDigest) || !bytes.Equal(current.RequestHash, requestHash) {
			return ErrNotReady
		}
		at, err := value.FromTime(now)
		if err != nil {
			return err
		}
		active, err := store.KeyActive(ctx, nodeID, current.ReceiptKey, at)
		if err != nil || !active {
			return ErrNotReady
		}
		changed, err := store.CompleteIssue(ctx, certificatestore.Issued{ID: request.CertificateID, ApprovalID: request.ApprovalID, Chain: result.CertificateChainPEM, RequestHash: requestHash, Serial: leaf.SerialNumber.String(), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), At: now})
		if err != nil {
			return err
		}
		if !changed {
			return ErrNotReady
		}
		return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID, Result: "succeeded", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"node_id":%q,"serial_number":%q,"not_after":%q}`, nodeID, leaf.SerialNumber.String(), leaf.NotAfter.UTC().Format(time.RFC3339))), At: now})
	})
	if err != nil {
		return Certificate{}, err
	}
	return s.Get(ctx, request.CertificateID)
}

func (s *Service) prepareIssue(ctx context.Context, request IssueRequest) (workspaceID, nodeID uuid.UUID, csrDER, requestHash []byte, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		stored, err := store.LockIssue(ctx, request.CertificateID)
		if errors.Is(err, database.ErrNotFound) {
			return ErrNotReady
		}
		if err != nil {
			return err
		}
		workspaceID, nodeID, csrDER = stored.WorkspaceID, stored.NodeID, stored.CSR
		var dnsNames []string
		if err := json.Unmarshal(stored.DNSNames.Bytes(), &dnsNames); err != nil {
			return ErrNotReady
		}
		csrDigest := sha256.Sum256(csrDER)
		subjectDigest, subjectErr := privdattestation.RequestedSubjectDigest(&agentv1.CertificateCsr{CertificateId: request.CertificateID[:], CommonName: stored.CommonName, DnsNames: dnsNames, KeyBits: stored.KeyBits})
		if !stored.ReceiptCurrent || len(stored.ReceiptDigest) != sha256.Size || len(stored.ReceiptKey) == 0 || !bytes.Equal(stored.CSRDigest, csrDigest[:]) || subjectErr != nil || !bytes.Equal(stored.SubjectDigest, subjectDigest) {
			return ErrNotReady
		}
		at, err := database.TransactionTime(ctx, tx)
		if err != nil {
			return err
		}
		active, err := store.KeyActive(ctx, nodeID, stored.ReceiptKey, at)
		if err != nil || !active {
			return ErrNotReady
		}
		switch stored.State {
		case "csr_ready":
			requestHash = certificateIssueBinding(request.CertificateID, stored.Version, csrDER, stored.ReceiptDigest)
			if err := approvals.ConsumeBoundTx(ctx, tx, request.ApprovalID, workspaceID, request.ActorIdentityID, "certificate.issue", "certificate", request.CertificateID, requestHash); err != nil {
				return err
			}
			now := s.now()
			if err := store.StartIssue(ctx, certificatestore.Signing{ID: request.CertificateID, ApprovalID: request.ApprovalID, ActorID: request.ActorIdentityID, RequestHash: requestHash, Version: stored.Version, At: now}); err != nil {
				return err
			}
			return audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: workspaceID, ActorType: "user", ActorID: request.ActorIdentityID.String(), SessionID: &request.ActorSessionID, Action: "certificate.issue", ResourceType: "certificate", ResourceID: request.CertificateID, NodeID: &nodeID, ApprovalID: &request.ApprovalID, RequestID: request.RequestID, Result: "intent", Reason: request.Reason, AfterSummary: json.RawMessage(fmt.Sprintf(`{"csr_sha256":%q,"state":"signing"}`, hex.EncodeToString(sha256Bytes(csrDER)))), At: now})
		case "signing", "signer_unavailable":
			if stored.IssueVersion == nil || *stored.IssueVersion < 1 {
				return ErrNotReady
			}
			requestHash = certificateIssueBinding(request.CertificateID, *stored.IssueVersion, csrDER, stored.ReceiptDigest)
			if stored.ApprovalID == nil || stored.ActorID == nil || *stored.ApprovalID != request.ApprovalID || *stored.ActorID != request.ActorIdentityID || !bytes.Equal(stored.RequestHash, requestHash) {
				return ErrNotReady
			}
			if stored.State == "signer_unavailable" {
				return store.ResumeIssue(ctx, request.CertificateID, s.now())
			}
			return nil
		default:
			return ErrNotReady
		}
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, nil, nil, err
	}
	return
}

func certificateIssueBinding(certificateID uuid.UUID, version int64, csrDER, receiptDigest []byte) []byte {
	value := []byte("ocservia/certificate-issue-approval/v2\x00")
	value = append(value, certificateID[:]...)
	value = binary.BigEndian.AppendUint64(value, uint64(version))
	value = append(value, sha256Bytes(csrDER)...)
	value = append(value, receiptDigest...)
	digest := sha256.Sum256(value)
	return digest[:]
}

func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (operationstore.Operation, bool, error) {
	if request.CertificateID == uuid.Nil || request.ApprovalID == uuid.Nil || request.CertificateVersion < 1 || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Reason) == "" || request.RequestID == "" {
		return operationstore.Operation{}, false, ErrInvalid
	}
	if s.signer == nil {
		return operationstore.Operation{}, false, ErrSignerUnavailable
	}
	certificate, err := s.Get(ctx, request.CertificateID)
	if err != nil {
		return operationstore.Operation{}, false, err
	}
	workspaceID, nodeID, serialNumber, state, certificateVersion := certificate.WorkspaceID, certificate.NodeID, certificate.SerialNumber, certificate.State, certificate.Version
	existingRequest, err := s.operationExists(ctx, workspaceID, request.IdempotencyKey)
	if err != nil {
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
		exists, err := s.operationExists(ctx, workspaceID, request.IdempotencyKey)
		if err != nil {
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
	if txErr := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.BeginRevocation(ctx, request.CertificateID, s.now())
	}); txErr != nil {
		return op, replay, txErr
	}
	if err := s.signer.Revoke(ctx, RevokeSignerRequest{CertificateID: request.CertificateID, SerialNumber: serialNumber, Reason: request.Reason}); err != nil {
		_ = database.Within(context.WithoutCancel(ctx), s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, storeErr := certificatestore.FromTransaction(tx)
			if storeErr != nil {
				return storeErr
			}
			return store.RevocationUnknown(context.WithoutCancel(ctx), request.CertificateID, s.now())
		})
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
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.ReleaseOperation(ctx, operationID)
	})
}

func (s *Service) operationExists(ctx context.Context, workspace uuid.UUID, key string) (exists bool, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		exists, err = store.OperationExists(ctx, workspace, key)
		return err
	})
	return
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
	var eligible certificatestore.ActionBinding
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		eligible, err = store.P12Eligible(ctx, request.CertificateID)
		return err
	})
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ArtifactGrant{}, false, ErrNotReady
		}
		return ArtifactGrant{}, false, err
	}
	workspaceID, nodeID, chain, serialNumber, certificateVersion := eligible.WorkspaceID, eligible.NodeID, eligible.Chain, eligible.Serial, eligible.Version
	if certificateVersion != request.CertificateVersion {
		return ArtifactGrant{}, false, ErrNotReady
	}
	intent, _ := json.Marshal(map[string]any{"certificate_id": request.CertificateID, "certificate_version": request.CertificateVersion, "artifact_request_id": request.ArtifactRequestID, "approval_id": request.ApprovalID, "node_id": nodeID, "expected_version": request.ExpectedVersion, "actor_identity_id": request.ActorIdentityID, "actor_session_id": request.ActorSessionID, "reason": request.Reason})
	intentHash := sha256.Sum256(intent)
	existing, err := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey, intentHash[:])
	if err == nil {
		if !existing.SameIntent {
			return ArtifactGrant{}, false, operationstore.ErrIdempotencyConflict
		}
		op, getErr := s.operations.Get(ctx, existing.OperationID)
		if getErr != nil {
			return ArtifactGrant{}, false, getErr
		}
		return ArtifactGrant{ArtifactID: existing.ID, Operation: op, ExpiresAt: existing.ExpiresAt}, true, nil
	}
	if !errors.Is(err, database.ErrNotFound) {
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
	if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		registeredKey, err = store.SealingKeyExists(ctx, nodeID, uint32(sealed.GetVersion()), sealed.GetKeyId())
		return err
	}); err != nil {
		return ArtifactGrant{}, false, err
	}
	if !registeredKey {
		return ArtifactGrant{}, false, ErrSignerUnavailable
	}
	artifactID := request.ArtifactRequestID
	expiresAt := s.now().Add(10 * time.Minute)
	expiry, err := value.FromTime(expiresAt)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	tokenHash := sha256.Sum256([]byte(token))
	approvalHash, _, err := certificateActionBinding("certificate.private_key.export", request.CertificateID, nodeID, certificateVersion, "certificate_p12", artifactID, serialNumber, chain)
	if err != nil {
		return ArtifactGrant{}, false, err
	}
	op, replay, err := s.operations.CreateSynthetic(ctx, operationstore.CreateRequest{NodeID: nodeID, ExpectedVersion: request.ExpectedVersion, IdempotencyKey: request.IdempotencyKey, Kind: operationstore.CertificateP12, CertificateID: request.CertificateID, CertificateVersion: uint64(certificateVersion), CertificateChain: chain, SealedPassword: sealed, ArtifactID: artifactID, ArtifactMetadata: &operationstore.ArtifactMetadata{TokenSHA256: tokenHash[:], RequestHash: intentHash[:], ExpiresAt: expiresAt}, ActorID: request.ActorIdentityID.String(), ActorIdentityID: request.ActorIdentityID, ActorSessionID: request.ActorSessionID, ApprovalID: request.ApprovalID, ApprovalRequestHash: approvalHash, Action: "certificate.private_key.export", Reason: request.Reason, RequestID: request.RequestID, Traceparent: request.Traceparent, TTL: 15 * time.Minute})
	if err != nil {
		if errors.Is(err, operationstore.ErrIdempotencyConflict) {
			found, lookupErr := s.artifactByRequest(ctx, workspaceID, request.IdempotencyKey, intentHash[:])
			if lookupErr == nil && found.SameIntent {
				existingOperation, getErr := s.operations.Get(ctx, found.OperationID)
				if getErr != nil {
					return ArtifactGrant{}, false, getErr
				}
				return ArtifactGrant{ArtifactID: found.ID, Operation: existingOperation, ExpiresAt: found.ExpiresAt}, true, nil
			}
		}
		return ArtifactGrant{}, false, err
	}
	if replay {
		operationID, err := uuid.Parse(op.ID)
		if err != nil {
			return ArtifactGrant{}, false, err
		}
		var found certificatestore.Artifact
		if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
			store, err := certificatestore.FromTransaction(tx)
			if err != nil {
				return err
			}
			found, err = store.ArtifactByOperation(ctx, operationID)
			return err
		}); err != nil {
			return ArtifactGrant{}, false, err
		}
		return ArtifactGrant{ArtifactID: found.ID, Operation: op, ExpiresAt: found.ExpiresAt}, true, nil
	}
	return ArtifactGrant{ArtifactID: artifactID, Operation: op, DownloadToken: token, Password: password, ExpiresAt: expiry}, false, nil
}

func (s *Service) artifactByRequest(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v certificatestore.Artifact, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		v, err = store.ArtifactByRequest(ctx, workspace, key, hash)
		return err
	})
	return
}

func (s *Service) ArtifactResource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		workspaceID, nodeID, err = store.Resource(ctx, id)
		return err
	})
	return
}

func (s *Service) OpenArtifact(ctx context.Context, id uuid.UUID, token string, subject uuid.UUID) (ArtifactDownload, error) {
	if id == uuid.Nil || subject == uuid.Nil || len(token) != 43 || s.artifacts == nil || s.grantSigner == nil {
		return ArtifactDownload{}, ErrArtifactDenied
	}
	tokenHash := sha256.Sum256([]byte(token))
	grantID := uuid.Must(uuid.NewV7())
	var eligible artifactstore.Eligible
	var grant *agentv1.ArtifactGrantV1
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err = store.LockCapacity(ctx); err != nil {
			return err
		}
		active, err := store.Active(ctx)
		if err != nil {
			return err
		}
		if active >= 4 {
			return ErrArtifactCapacity
		}
		eligible, err = store.Eligible(ctx, id, tokenHash[:])
		if err != nil {
			return ErrArtifactDenied
		}
		if eligible.RequesterID != subject {
			return ErrArtifactDenied
		}
		issuedAt := s.now().UTC()
		expires := issuedAt.Add(time.Minute)
		for _, deadline := range []value.Timestamp{eligible.ArtifactExpires, eligible.CertificateExpires} {
			if !deadline.Valid || deadline.Validate() != nil {
				return ErrArtifactDenied
			}
			if deadline.Micros == value.PositiveInfinity {
				continue
			}
			finite, timeErr := deadline.Time()
			if timeErr != nil {
				return ErrArtifactDenied
			}
			if finite.Before(expires) {
				expires = finite
			}
		}
		grant, err = s.grantSigner.IssueArtifactGrant(eligible.NodeID, id, eligible.CertificateID, eligible.CertificateVersion, eligible.OperationID, eligible.RequesterID.String(), uint64(eligible.Size), grantID, issuedAt, expires)
		if err != nil {
			return err
		}
		return store.Lease(ctx, id, grantID, subject, expires)
	})
	if err != nil {
		return ArtifactDownload{}, err
	}
	var reader io.ReadCloser
	if err := s.executeArtifactFenced(ctx, eligible.NodeID, id,
		func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			fetched, err := s.artifacts.FetchArtifact(ctx, grant, binding)
			if err != nil {
				return err
			}
			reader = fetched
			return nil
		}); err != nil {
		_ = s.AbortArtifact(context.WithoutCancel(ctx), id, grantID)
		return ArtifactDownload{}, err
	}
	return ArtifactDownload{Reader: reader, ExpectedSHA256: eligible.Digest, Size: eligible.Size, NodeID: eligible.NodeID, GrantID: grantID, Grant: grant}, nil
}

func (s *Service) CompleteArtifact(ctx context.Context, id, grantID uuid.UUID, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, actorID, sessionID uuid.UUID, requestID string) error {
	if grant == nil || actorID == uuid.Nil || sessionID == uuid.Nil || requestID == "" || len(requestID) > 128 || grant.GetVersion() != agentv1.ArtifactGrantVersion_ARTIFACT_GRANT_VERSION_V1 || grant.GetPurpose() != "certificate_p12" || grant.GetCertificateVersion() == 0 || grant.GetAuthorizedSubject() != actorID.String() || !bytes.Equal(grant.GetArtifactId(), id[:]) || !bytes.Equal(grant.GetGrantId(), grantID[:]) || len(digest) != sha256.Size || size < 1 || uint64(size) != grant.GetMaxBytes() {
		return ErrArtifactDenied
	}
	nodeID, nodeErr := uuid.FromBytes(grant.GetNodeId())
	certificateID, certificateErr := uuid.FromBytes(grant.GetCertificateId())
	operationID, operationErr := uuid.FromBytes(grant.GetOperationId())
	if nodeErr != nil || certificateErr != nil || operationErr != nil || nodeID.Version() != 7 || certificateID.Version() != 7 || operationID.Version() != 7 {
		return ErrArtifactDenied
	}
	grantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(grant)
	if err != nil || len(grantBytes) == 0 || len(grantBytes) > 4096 {
		return ErrArtifactDenied
	}
	consumption := artifactstore.Consumption{ID: id, GrantID: grantID, ActorID: actorID, SessionID: sessionID, NodeID: nodeID, CertificateID: certificateID, OperationID: operationID, CertificateVersion: grant.GetCertificateVersion(), Grant: grantBytes, Digest: digest, Size: size, RequestID: requestID}
	exactReplay := false
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		changed, err := store.StartConsumption(ctx, consumption)
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
		exactReplay, err = store.ExactReplay(ctx, consumption)
		if err != nil {
			return err
		}
		if !exactReplay {
			return ErrArtifactDenied
		}
		return nil
	})
	if err != nil {
		return err
	}
	if exactReplay {
		return nil
	}
	// Run the root consume inside the owner fencing interval: the binding is
	// signed and the mutation RPC completes before the ownership guard is
	// released. Recovery can later confirm this same root record without
	// authorizing a new mutation after the grant expires.
	if err := s.executeArtifactFenced(ctx, nodeID, grantID,
		func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
			return s.artifacts.ConsumeArtifact(ctx, grant, digest, size, binding)
		}); err != nil {
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
	result := artifactConsumptionFinalized
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		v, err := store.Finalize(ctx, id, grantID)
		if errors.Is(err, database.ErrNotFound) {
			state, err := store.State(ctx, id, grantID)
			if err != nil {
				return ErrArtifactDenied
			}
			switch state {
			case "consumed":
				result = artifactConsumptionAlreadyFinalized
			case "revoked":
				result = artifactConsumptionRevoked
			case "expired":
				result = artifactConsumptionExpired
			default:
				return ErrArtifactDenied
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err = audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: v.WorkspaceID, ActorType: "user", ActorID: v.ActorID.String(), SessionID: &v.SessionID, Action: "certificate.p12.download", ResourceType: "artifact", ResourceID: id, NodeID: &v.NodeID, RequestID: v.RequestID, Result: "succeeded", AfterSummary: json.RawMessage(fmt.Sprintf(`{"certificate_id":%q,"sha256":%q,"size":%d}`, v.CertificateID, hex.EncodeToString(v.Digest), v.Size)), At: s.now()}); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
	return result, err
}

func (s *Service) reconcileConsumingArtifacts(ctx context.Context) error {
	var records []artifactstore.PendingConsumption
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		records, err = store.PendingConsumptions(ctx)
		return err
	})
	if err != nil {
		return err
	}
	type pending struct {
		id, grantID uuid.UUID
		grant       *agentv1.ArtifactGrantV1
		digest      []byte
		size        int64
		actorID     uuid.UUID
		expiresAt   value.Timestamp
	}
	values := make([]pending, 0, 20)
	for _, record := range records {
		value := pending{id: record.ID, grantID: record.GrantID, grant: &agentv1.ArtifactGrantV1{}, digest: record.Digest, size: record.Size, actorID: record.ActorID, expiresAt: record.ExpiresAt}
		if err := proto.Unmarshal(record.Grant, value.grant); err != nil || !bytes.Equal(value.grant.GetArtifactId(), value.id[:]) || !bytes.Equal(value.grant.GetGrantId(), value.grantID[:]) || value.grant.GetAuthorizedSubject() != value.actorID.String() || value.grant.GetMaxBytes() != uint64(value.size) {
			return ErrArtifactDenied
		}
		values = append(values, value)
	}
	for _, item := range values {
		recoveryNode, nodeErr := uuid.FromBytes(item.grant.GetNodeId())
		if nodeErr != nil {
			continue
		}
		var consumed bool
		confirmErr := s.executeArtifactFenced(ctx, recoveryNode, item.grantID,
			func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				confirmed, err := s.artifacts.ConfirmArtifactConsumed(ctx, item.grant, item.digest, item.size, binding)
				consumed = confirmed
				return err
			})
		if confirmErr != nil {
			// Reconciliation is item-scoped. A disconnected node or restarting
			// Agent must not turn one durable artifact into a global scheduler
			// failure; exact evidence remains in consuming for the next pass.
			continue
		}
		grantExpires := item.grant.GetExpiresAt()
		if grantExpires == nil {
			return ErrArtifactDenied
		}
		if !consumed && grantExpires.AsTime().After(s.now()) {
			if err := s.executeArtifactFenced(ctx, recoveryNode, item.grantID,
				func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
					return s.artifacts.ConsumeArtifact(ctx, item.grant, item.digest, item.size, binding)
				}); err != nil {
				continue
			}
			consumed = true
		}
		if consumed {
			// Revocation and expiry may win the terminal transition after root
			// confirmation. They are benign for reconciliation, but the explicit
			// result prevents an in-flight HTTP request from treating them as a
			// successful delivery.
			if _, err := s.finalizeArtifactConsumption(ctx, item.id, item.grantID); err != nil {
				return err
			}
		} else {
			now, err := value.FromTime(s.now())
			if err != nil {
				return err
			}
			if !item.expiresAt.Valid {
				return ErrArtifactDenied
			}
			if err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
				store, err := artifactstore.FromTransaction(tx)
				if err != nil {
					return err
				}
				if err := store.ResetConsumption(ctx, item.id, item.grantID, item.expiresAt.Micros <= now.Micros); err != nil {
					return err
				}
				return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) AbortArtifact(ctx context.Context, id, grantID uuid.UUID) error {
	// Preserve the durable lease until its bounded expiry. A transport failure
	// must not make a second concurrently valid grant immediately issuable.
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := artifactstore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.Abort(ctx, id, grantID)
	})
}

func (s *Service) Maintain(ctx context.Context) error {
	if err := s.reconcileConsumingArtifacts(ctx); err != nil {
		return err
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		if err := store.Maintain(ctx, s.now()); err != nil {
			return err
		}
		return coordination.AssertFenceTx(ctx, tx, coordination.FenceFromContext(ctx))
	})
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Certificate, error) {
	var result certificatestore.Certificate
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		result, err = store.Get(ctx, id)
		return err
	})
	return readCertificate(result), err
}

func (s *Service) ListNode(ctx context.Context, nodeID uuid.UUID) ([]Certificate, error) {
	values := make([]Certificate, 0)
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		rows, err := store.ListNode(ctx, nodeID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			values = append(values, readCertificate(row))
		}
		return nil
	})
	return values, err
}

func (s *Service) GetByOperation(ctx context.Context, operationID uuid.UUID, replay bool) (Certificate, bool, error) {
	var result certificatestore.Certificate
	err := database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		result, err = store.GetByOperation(ctx, operationID)
		return err
	})
	return readCertificate(result), replay, err
}

func (s *Service) Resource(ctx context.Context, id uuid.UUID) (workspaceID, nodeID uuid.UUID, err error) {
	err = database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		workspaceID, nodeID, err = store.Resource(ctx, id)
		return err
	})
	return
}

func readCertificate(v certificatestore.Certificate) Certificate {
	optional := func(at value.Timestamp) *value.Timestamp {
		if !at.Valid {
			return nil
		}
		return &at
	}
	return Certificate{ID: v.ID, WorkspaceID: v.WorkspaceID, NodeID: v.NodeID, OperationID: v.OperationID, CommonName: v.CommonName, DNSNames: v.DNSNames.Bytes(), KeyBits: v.KeyBits, State: v.State, Version: v.Version, PublicKeySHA256: v.PublicKeySHA256, SerialNumber: v.SerialNumber, NotBefore: optional(v.NotBefore), NotAfter: optional(v.NotAfter), RevokedAt: optional(v.RevokedAt), CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
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
