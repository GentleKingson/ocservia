// Package artifactstore defines the durable certificate download transaction.
package artifactstore

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"time"
)

type Eligible struct {
	NodeID, CertificateID, OperationID, RequesterID uuid.UUID
	CertificateVersion                              uint64
	Digest                                          []byte
	Size                                            int64
	ArtifactExpires, CertificateExpires             time.Time
}
type Consumption struct {
	ID, GrantID, ActorID, SessionID, NodeID, CertificateID, OperationID uuid.UUID
	CertificateVersion                                                  uint64
	Grant, Digest                                                       []byte
	Size                                                                int64
	RequestID                                                           string
}
type Finalized struct {
	WorkspaceID, NodeID, CertificateID, ActorID, SessionID uuid.UUID
	Digest                                                 []byte
	Size                                                   int64
	RequestID                                              string
}
type Store interface {
	Resource(context.Context, uuid.UUID) (uuid.UUID, uuid.UUID, error)
	LockCapacity(context.Context) error
	Active(context.Context) (int, error)
	Eligible(context.Context, uuid.UUID, []byte) (Eligible, error)
	Lease(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) error
	StartConsumption(context.Context, Consumption) (bool, error)
	ExactReplay(context.Context, Consumption) (bool, error)
	Finalize(context.Context, uuid.UUID, uuid.UUID) (Finalized, error)
	State(context.Context, uuid.UUID, uuid.UUID) (string, error)
	Abort(context.Context, uuid.UUID, uuid.UUID) error
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ ArtifactStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ArtifactStore(), nil
}
