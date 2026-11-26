package store

import (
	"time"

	"github.com/google/uuid"
)

const ReconciliationAttemptLimit = 64

type RecoveryCommand struct {
	CommandID, OperationID, NodeID, OutboxID, AttemptID uuid.UUID
	Envelope                                            []byte
	CommandState, OperationState                        string
	Attempts, AttemptLimit                              int
	ExpiresAt                                           time.Time
}

type RecoveryUpdate struct {
	Command       RecoveryCommand
	Envelope      []byte
	ExpiresAt, At time.Time
	Schedule      bool
}
