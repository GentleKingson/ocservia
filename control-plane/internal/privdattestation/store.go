// Store contract shared by the storage contract shared by the privd
// attestation enrollment, registration and revocation transactions.
package privdattestation

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

// Key is one registered attestation key row.
type Key struct {
	NodeID                uuid.UUID
	KeyID                 string
	PublicKey             []byte
	CreatedAt, ApprovedAt time.Time
	ActivatedAt           time.Time
	ValidUntil            *time.Time
	PredecessorKeyID      *string
}

// VerificationKey preserves the business-finite validity contract used by
// receipt verification. NULL ValidUntil means unlimited validity.
type VerificationKey struct {
	PublicKey   []byte
	State       string
	ActivatedAt time.Time
	ValidUntil  *time.Time
}

type Store interface {
	FirstKey(context.Context, uuid.UUID) (string, VerificationKey, error)
	KeyStateCounts(context.Context) (database.Rows, error)
	VerificationKey(context.Context, uuid.UUID, string) (VerificationKey, error)
	InsertKey(context.Context, Key) error
	ApproveCapability(context.Context, uuid.UUID, string) error
	BumpNodeRevision(context.Context, uuid.UUID, time.Time) error
}

type Provider interface{ PrivdAttestationStore() Store }

func StoreFrom(tx database.Tx) (Store, error) {
	p, ok := tx.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.PrivdAttestationStore(), nil
}
