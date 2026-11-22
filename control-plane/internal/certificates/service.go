package certificates

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
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
	backend     database.Backend
	operations  *operations.Service
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
	ID              uuid.UUID       `json:"id"`
	WorkspaceID     uuid.UUID       `json:"workspace_id"`
	NodeID          uuid.UUID       `json:"node_id"`
	OperationID     uuid.UUID       `json:"operation_id"`
	CommonName      string          `json:"common_name"`
	DNSNames        json.RawMessage `json:"dns_names"`
	KeyBits         uint32          `json:"key_bits"`
	State           string          `json:"state"`
	Version         int64           `json:"version"`
	PublicKeySHA256 []byte          `json:"public_key_sha256,omitempty"`
	SerialNumber    string          `json:"serial_number,omitempty"`
	NotBefore       *time.Time      `json:"not_before,omitempty"`
	NotAfter        *time.Time      `json:"not_after,omitempty"`
	RevokedAt       *time.Time      `json:"revoked_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type IssueRequest struct {
	CertificateID, ActorIdentityID, ActorSessionID uuid.UUID
	Reason, RequestID                              string
}

type RevokeRequest struct {
	CertificateID, ActorIdentityID, ActorSessionID uuid.UUID
	CertificateVersion                             int64
	ExpectedVersion                                int64
	IdempotencyKey, Reason, RequestID, Traceparent string
}

type P12Request struct {
	CertificateID, ArtifactRequestID, ActorIdentityID, ActorSessionID uuid.UUID
	CertificateVersion                                                int64
	ExpectedVersion                                                   int64
	IdempotencyKey, Reason, RequestID, Traceparent                    string
}

type ArtifactGrant struct {
	ArtifactID    uuid.UUID            `json:"artifact_id"`
	Operation     operations.Operation `json:"operation"`
	DownloadToken string               `json:"download_token,omitempty"`
	Password      string               `json:"password,omitempty"`
	ExpiresAt     time.Time            `json:"expires_at"`
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
func NewBackend(backend database.Backend, operationService *operations.Service, signer Signer, sealer SecretSealer, artifacts ArtifactFetcher, grantSigner *commandauth.Signer) *Service {
	return &Service{backend: backend, operations: operationService, signer: signer, sealer: sealer, artifacts: artifacts, grantSigner: grantSigner, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Certificate, bool, error) {
	if request.NodeID == uuid.Nil || request.ActorIdentityID == uuid.Nil || request.ActorSessionID == uuid.Nil || request.ExpectedVersion < 1 || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.CommonName) == "" || strings.TrimSpace(request.Reason) == "" {
		return Certificate{}, false, ErrInvalid
	}
	certificateID, err := uuid.NewV7()
	if err != nil {
		return Certificate{}, false, err
	}
	op, replay, err := s.operations.Create(ctx, operations.CreateRequest{
		NodeID:          request.NodeID,
		ExpectedVersion: request.ExpectedVersion,
		IdempotencyKey:  request.IdempotencyKey,
		ActorID:         request.ActorID,
		ActorIdentityID: request.ActorIdentityID,
		ActorSessionID:  request.ActorSessionID,
		Action:          "certificate.issue",
		Reason:          request.Reason,
		RequestID:       request.RequestID,
		Traceparent:     request.Traceparent,
		TTL:             15 * time.Minute,
		Payload: operations.CertificateCSRPayload{
			CertificateID: certificateID,
			CommonName:    request.CommonName,
			DNSNames:      request.DNSNames,
			KeyBits:       request.KeyBits,
		},
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

func (s *Service) Maintain(ctx context.Context) error {
	if err := s.reconcileConsumingArtifacts(ctx); err != nil {
		return err
	}
	return database.Within(ctx, s.backend, database.ReadCommitted, func(tx database.Tx) error {
		store, err := certificatestore.FromTransaction(tx)
		if err != nil {
			return err
		}
		return store.Maintain(ctx, s.now())
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
	return Certificate{ID: v.ID, WorkspaceID: v.WorkspaceID, NodeID: v.NodeID, OperationID: v.OperationID, CommonName: v.CommonName, DNSNames: v.DNSNames, KeyBits: v.KeyBits, State: v.State, Version: v.Version, PublicKeySHA256: v.PublicKeySHA256, SerialNumber: v.SerialNumber, NotBefore: v.NotBefore, NotAfter: v.NotAfter, RevokedAt: v.RevokedAt, CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC()}
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
