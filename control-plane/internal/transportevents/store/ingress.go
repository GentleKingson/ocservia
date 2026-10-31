package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type IngressTrust struct {
	WorkspaceID               uuid.UUID
	NodeStatus, EndpointState string
}
type TransportEvent struct {
	ID, NodeID        uuid.UUID
	Type, Traceparent string
	Payload           []byte
	At                value.Timestamp
}
type Quarantine struct {
	EventID, NodeID          uuid.UUID
	Type                     int32
	PayloadHash              []byte
	ReasonCode, ReasonDetail string
	At                       value.Timestamp
}
type IngressStore interface {
	SaveBusiness(context.Context) error
	RollbackBusiness(context.Context) error
	ReleaseBusiness(context.Context) error
	LockTrust(context.Context, uuid.UUID, []byte) (IngressTrust, error)
	InsertEvent(context.Context, TransportEvent) (bool, error)
	NodeStatus(context.Context, uuid.UUID, string, value.Timestamp) error
	SimulationOutcome(context.Context, uuid.UUID, string, string, bool, value.Timestamp) error
	InsertQuarantine(context.Context, Quarantine) (bool, error)
	Quarantine(context.Context, uuid.UUID) (Quarantine, error)
	QuarantineAlert(context.Context, uuid.UUID, uuid.UUID, Quarantine) error
	AdvanceCursor(context.Context, uuid.UUID, value.Timestamp) error
	LastEventID(context.Context) (uuid.UUID, error)
}

func Ingress(tx database.Tx) (IngressStore, error) {
	if p, ok := tx.(interface{ TransportIngressStore() IngressStore }); ok {
		return p.TransportIngressStore(), nil
	}
	return nil, database.ErrUnsupported
}
