package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type AmbiguousDispatch struct {
	OperationID uuid.UUID
	Envelope    []byte
}

type RecoveryProjection struct {
	OperationID, CertificateID uuid.UUID
	ConfigApply, Artifact      bool
	At                         time.Time
}

type RecoveryStore interface {
	ReconnectAuthority(context.Context, uuid.UUID, uuid.UUID, int64) error
	AmbiguousDispatches(context.Context, uuid.UUID, int) ([]uuid.UUID, error)
	LockAmbiguousDispatch(context.Context, uuid.UUID, uuid.UUID) (AmbiguousDispatch, error)
	UpgradeSchedulingAcked(context.Context, uuid.UUID) (bool, error)
	MarkRecoveryProjection(context.Context, RecoveryProjection) error
	ScheduleReconnectRecovery(context.Context, uuid.UUID, uuid.UUID, []byte, time.Time, time.Time) error
}
