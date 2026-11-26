package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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
	At                time.Time
}
type Quarantine struct {
	EventID, NodeID          uuid.UUID
	Type                     int32
	PayloadHash              []byte
	ReasonCode, ReasonDetail string
	At                       time.Time
}
type IngressStore interface {
	SaveBusiness(context.Context) error
	RollbackBusiness(context.Context) error
	ReleaseBusiness(context.Context) error
	LockTrust(context.Context, uuid.UUID, []byte) (IngressTrust, error)
	LockTrustShared(context.Context, uuid.UUID, []byte) (IngressTrust, error)
	InsertEvent(context.Context, TransportEvent) (bool, error)
	NodeStatus(context.Context, uuid.UUID, string, time.Time) error
	SimulationOutcome(context.Context, uuid.UUID, string, string, bool, time.Time) error
	InsertQuarantine(context.Context, Quarantine) (bool, error)
	Quarantine(context.Context, uuid.UUID) (Quarantine, error)
	QuarantineAlert(context.Context, uuid.UUID, uuid.UUID, Quarantine) error
	AdvanceCursor(context.Context, uuid.UUID, time.Time) error
	LastEventID(context.Context) (uuid.UUID, error)
}

func Ingress(tx database.Tx) IngressStore {
	return database.Must[interface{ TransportIngressStore() IngressStore }](tx).TransportIngressStore()
}
