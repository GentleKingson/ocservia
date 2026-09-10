// Package store defines certificate persistence operations on a common transaction.
package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Certificate struct {
	ID, WorkspaceID, NodeID, OperationID                 uuid.UUID
	CommonName, State, SerialNumber                      string
	DNSNames                                             value.JSONB
	KeyBits                                              uint32
	Version                                              int64
	PublicKeySHA256                                      []byte
	NotBefore, NotAfter, RevokedAt, CreatedAt, UpdatedAt value.Timestamp
}

type IssueState struct {
	WorkspaceID, NodeID                                       uuid.UUID
	CSR, RequestHash, ReceiptDigest, CSRDigest, SubjectDigest []byte
	State, ReceiptKey, CommonName                             string
	ApprovalID, ActorID                                       *uuid.UUID
	Version                                                   int64
	IssueVersion                                              *int64
	ReceiptCurrent                                            bool
	DNSNames                                                  value.JSONB
	KeyBits                                                   uint32
}

type IssueReceipt struct {
	NodeID                                                    uuid.UUID
	CSR, ReceiptDigest, CSRDigest, SubjectDigest, RequestHash []byte
	ReceiptKey                                                string
	Current, KeyActive                                        bool
}

type Signing struct {
	ID, ApprovalID, ActorID uuid.UUID
	RequestHash             []byte
	Version                 int64
	At                      time.Time
}

type Issued struct {
	ID, ApprovalID          uuid.UUID
	Chain, RequestHash      []byte
	Serial                  string
	NotBefore, NotAfter, At time.Time
}

type ActionBinding struct {
	WorkspaceID, NodeID uuid.UUID
	Version             int64
	Serial              string
	Chain               []byte
}

type Artifact struct {
	ID, OperationID uuid.UUID
	ExpiresAt       value.Timestamp
	SameIntent      bool
}

type PendingCSR struct {
	ID, WorkspaceID, NodeID, OperationID uuid.UUID
	CommonName                           string
	DNSNames                             value.JSONB
	KeyBits                              uint32
	CreatedAt                            value.Timestamp
}

type PendingArtifact struct {
	ID, WorkspaceID, NodeID, CertificateID, OperationID, ApprovalID uuid.UUID
	CertificateVersion                                              uint64
	TokenSHA256, RequestHash                                        []byte
	ExpiresAt, CreatedAt                                            value.Timestamp
}

type SecretReference struct {
	ID, WorkspaceID                   uuid.UUID
	Provider, KeyPath, Version, State string
	RotatedAt, CreatedAt, UpdatedAt   value.Timestamp
}

type Store interface {
	InsertCSR(context.Context, PendingCSR) error
	InsertArtifact(context.Context, PendingArtifact) error
	Maintain(context.Context, time.Time) error
	Get(context.Context, uuid.UUID) (Certificate, error)
	GetByOperation(context.Context, uuid.UUID) (Certificate, error)
	ListNode(context.Context, uuid.UUID) ([]Certificate, error)
	Resource(context.Context, uuid.UUID) (uuid.UUID, uuid.UUID, error)
	LockIssue(context.Context, uuid.UUID) (IssueState, error)
	ApprovedReceipt(context.Context, uuid.UUID, uuid.UUID) (IssueReceipt, error)
	LockReceipt(context.Context, uuid.UUID) (IssueReceipt, error)
	KeyActive(context.Context, uuid.UUID, string, value.Timestamp) (bool, error)
	StartIssue(context.Context, Signing) error
	ResumeIssue(context.Context, uuid.UUID, time.Time) error
	SignerUnavailable(context.Context, uuid.UUID, uuid.UUID, []byte, time.Time) error
	CompleteIssue(context.Context, Issued) (bool, error)
	IssueBinding(context.Context, uuid.UUID) (IssueState, error)
	ActionBinding(context.Context, uuid.UUID) (ActionBinding, error)
	OperationExists(context.Context, uuid.UUID, string) (bool, error)
	BeginRevocation(context.Context, uuid.UUID, time.Time) error
	RevocationUnknown(context.Context, uuid.UUID, time.Time) error
	ReleaseOperation(context.Context, uuid.UUID) error
	P12Eligible(context.Context, uuid.UUID) (ActionBinding, error)
	ArtifactByRequest(context.Context, uuid.UUID, string, []byte) (Artifact, error)
	ArtifactByOperation(context.Context, uuid.UUID) (Artifact, error)
	SealingKeyExists(context.Context, uuid.UUID, uint32, string) (bool, error)
	InsertSecretReference(context.Context, SecretReference) error
	RotateSecretReference(context.Context, uuid.UUID, string, value.Timestamp) (SecretReference, error)
	GetSecretReference(context.Context, uuid.UUID) (SecretReference, error)
	SecretReferenceResource(context.Context, uuid.UUID) (uuid.UUID, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ CertificateStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.CertificateStore(), nil
}
