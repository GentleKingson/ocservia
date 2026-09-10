package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type RecoveryAuthority struct {
	OwnerID     uuid.UUID
	Incarnation int64
}

type AmbiguousDispatch struct {
	OperationID uuid.UUID
	Envelope    []byte
}

type RecoveryProjection struct {
	OperationID, CertificateID uuid.UUID
	ConfigApply, Artifact      bool
	At                         value.Timestamp
}

type RecoveryStore interface {
	ReconnectAuthority(context.Context, uuid.UUID, uuid.UUID, int64) (RecoveryAuthority, error)
	AmbiguousDispatches(context.Context, uuid.UUID, int) ([]uuid.UUID, error)
	LockAmbiguousDispatch(context.Context, uuid.UUID, uuid.UUID) (AmbiguousDispatch, error)
	UpgradeSchedulingAcked(context.Context, uuid.UUID) (bool, error)
	MarkRecoveryProjection(context.Context, RecoveryProjection) error
	ScheduleReconnectRecovery(context.Context, uuid.UUID, uuid.UUID, []byte, value.Timestamp, value.Timestamp) error
}
