package store

import (
	"time"

	"github.com/google/uuid"
)

type AmbiguousDispatch struct {
	OperationID uuid.UUID
	Envelope    []byte
	ExpiresAt   time.Time
}

type RecoveryProjection struct {
	OperationID, CertificateID uuid.UUID
	ConfigApply, Artifact      bool
	At                         time.Time
}
