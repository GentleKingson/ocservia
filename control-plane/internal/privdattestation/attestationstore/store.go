// Package attestationstore owns the storage contract shared by the privd
// attestation enrollment, registration and revocation transactions.
package attestationstore

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

// Credential is one enrollment credential row as the registration flow reads
// and writes it. Every time is a business-finite instant; unlimited key
// validity is represented by a nil ValidUntil, never by an infinity.
type Credential struct {
	ID                  uuid.UUID
	NodeID              uuid.UUID
	SecretSHA256        []byte
	ControllerNonce     []byte
	ContextSHA256       []byte
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
	CreatedByIdentityID uuid.UUID
	CreatedBySessionID  uuid.UUID
	CreatedAt           time.Time
}

// Key is one registered attestation key row. Rotation bounds the predecessor
// with a finite ValidUntil; ApprovedAt and ActivatedAt record the same
// credential-consumption instant.
type Key struct {
	NodeID                   uuid.UUID
	KeyID                    string
	PublicKey                []byte
	CreatedAt, ApprovedAt    time.Time
	ActivatedAt              time.Time
	ValidUntil               *time.Time
	PredecessorKeyID         *string
	RegistrationCredentialID uuid.UUID
}

type Store interface {
	// LockActiveNode takes the node row lock for an active or offline node and
	// returns its workspace.
	LockActiveNode(context.Context, uuid.UUID) (uuid.UUID, error)
	// LockNode takes the node row lock regardless of status.
	LockNode(context.Context, uuid.UUID) (uuid.UUID, error)
	// OutstandingCredential reports an unconsumed, unexpired credential.
	OutstandingCredential(context.Context, uuid.UUID, time.Time) (bool, error)
	InsertCredential(context.Context, Credential) error
	// LockCredential locks the credential row matching the secret digest.
	LockCredential(context.Context, []byte) (Credential, error)
	// ActiveKeys returns the node's active keys with unexpired validity,
	// oldest activation first, locked for the registration transaction.
	ActiveKeys(context.Context, uuid.UUID, time.Time) ([]string, error)
	InsertKey(context.Context, Key) error
	// RotatePredecessor bounds the predecessor's validity and names its
	// successor.
	RotatePredecessor(context.Context, uuid.UUID, string, string, time.Time) error
	ConsumeCredential(context.Context, uuid.UUID, time.Time) error
	// ApproveCapability records the capability granted by credential
	// consumption.
	ApproveCapability(context.Context, uuid.UUID, string) error
	// BumpNodeRevision advances the node's authorization revision.
	BumpNodeRevision(context.Context, uuid.UUID, time.Time) error
	// RevokeKey revokes one active key and bounds its validity; it reports
	// whether the exact key was still active.
	RevokeKey(context.Context, uuid.UUID, string, time.Time) (bool, error)
}

type Provider interface{ PrivdAttestationStore() Store }

func From(tx database.Tx) (Store, error) {
	p, ok := tx.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.PrivdAttestationStore(), nil
}
