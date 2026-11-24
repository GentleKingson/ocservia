// Package store defines certificate persistence operations on a common transaction.
package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Certificate struct {
	ID, WorkspaceID, NodeID, OperationID uuid.UUID
	CommonName, State, SerialNumber      string
	DNSNames                             []byte
	KeyBits                              uint32
	Version                              int64
	PublicKeySHA256                      []byte
	NotBefore, NotAfter, RevokedAt       *time.Time
	CreatedAt, UpdatedAt                 time.Time
}

type IssueState struct {
	WorkspaceID, NodeID           uuid.UUID
	CSR, ReceiptDigest, CSRDigest []byte
	State, ReceiptKey, CommonName string
	ActorID                       *uuid.UUID
	Version                       int64
	IssueVersion                  *int64
	ReceiptCurrent                bool
	DNSNames                      []byte
	KeyBits                       uint32
}

type IssueReceipt struct {
	NodeID                        uuid.UUID
	CSR, ReceiptDigest, CSRDigest []byte
	ReceiptKey                    string
	Current, KeyActive            bool
	IssueVersion                  int64
}

type Signing struct {
	ID, ActorID uuid.UUID
	Version     int64
	At          time.Time
}

type Issued struct {
	ID                      uuid.UUID
	Chain                   []byte
	IssueVersion            int64
	Serial                  string
	NotBefore, NotAfter, At time.Time
}

type PendingRevocation struct {
	OperationID, CommandID, WorkspaceID, NodeID uuid.UUID
	Envelope                                    []byte
}

type ActionBinding struct {
	WorkspaceID, NodeID uuid.UUID
	Version             int64
	Serial              string
	Chain               []byte
}

type Artifact struct {
	ID, OperationID uuid.UUID
	ExpiresAt       time.Time
	RequestHash     []byte
}

type PendingCSR struct {
	ID, WorkspaceID, NodeID, OperationID uuid.UUID
	CommonName                           string
	DNSNames                             []byte
	KeyBits                              uint32
	CreatedAt                            time.Time
}

type PendingArtifact struct {
	ID, WorkspaceID, NodeID, CertificateID, OperationID, RequesterID uuid.UUID
	CertificateVersion                                               uint64
	TokenSHA256                                                      []byte
	ExpiresAt, CreatedAt                                             time.Time
}

type SecretReference struct {
	ID, WorkspaceID                   uuid.UUID
	Provider, KeyPath, Version, State string
	RotatedAt                         *time.Time
	CreatedAt, UpdatedAt              time.Time
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
	SigningReceipt(context.Context, uuid.UUID) (IssueReceipt, error)
	LockReceipt(context.Context, uuid.UUID) (IssueReceipt, error)
	KeyActive(context.Context, uuid.UUID, string, time.Time) (bool, error)
	StartIssue(context.Context, Signing) error
	ResumeIssue(context.Context, uuid.UUID, time.Time) error
	SignerUnavailable(context.Context, uuid.UUID, int64, time.Time) error
	CompleteIssue(context.Context, Issued) (bool, error)
	OperationExists(context.Context, uuid.UUID, string) (bool, error)
	ReserveRevocation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64, time.Time) (bool, error)
	PendingRevocations(context.Context, *uuid.UUID) ([]PendingRevocation, error)
	ExpiredRevocations(context.Context) ([]PendingRevocation, error)
	ExpireRevocation(context.Context, uuid.UUID, uuid.UUID, uint64, []byte, time.Time) error
	RevocationSerial(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64) (string, error)
	RevocationUnknown(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	ReleaseOperation(context.Context, uuid.UUID, uuid.UUID) error
	P12Eligible(context.Context, uuid.UUID) (ActionBinding, error)
	ArtifactByRequest(context.Context, uuid.UUID, string) (Artifact, error)
	SealingKeyExists(context.Context, uuid.UUID, uint32, string) (bool, error)
	InsertSecretReference(context.Context, SecretReference) error
	RotateSecretReference(context.Context, uuid.UUID, string, time.Time) (SecretReference, error)
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
