package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type TrustJob struct {
	NodeID        uuid.UUID
	EndpointID    []byte
	DesiredState  string
	Revision      uint64
	Reason        string
	UpdateApplied bool
	CloseRequired bool
	CloseApplied  bool
	Attempts      int
}

// Enqueue only replaces older revisions. Completion and release are scoped to
// the claimed node, revision and worker, so superseding intent keeps its state.
type TrustStore interface {
	Enqueue(context.Context, TrustJob, value.Timestamp) error
	Claim(context.Context, uuid.UUID) (TrustJob, error)
	MarkUpdateApplied(context.Context, TrustJob, uuid.UUID) (bool, error)
	MarkCloseApplied(context.Context, TrustJob, uuid.UUID) (bool, error)
	UnlockComplete(context.Context, TrustJob, uuid.UUID) error
	Release(context.Context, TrustJob, uuid.UUID, time.Duration, string) error
}

func Trust(tx database.Tx) (TrustStore, error) {
	p, ok := tx.(interface{ TrustConvergenceStore() TrustStore })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.TrustConvergenceStore(), nil
}
