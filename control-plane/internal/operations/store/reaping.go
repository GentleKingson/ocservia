package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type RecoveryCommand struct {
	CommandID, OperationID, NodeID, OutboxID, AttemptID uuid.UUID
	Envelope                                            []byte
	CommandState, OperationState                        string
	Attempts                                            int
	ExpiresAt                                           time.Time
}

type RecoveryUpdate struct {
	Command       RecoveryCommand
	Envelope      []byte
	ExpiresAt, At time.Time
	Schedule      bool
}

type ReapingStore interface {
	RecoveryClock(context.Context) (time.Time, error)
	ExpiredApplyCommands(context.Context) ([]RecoveryCommand, error)
	ReconcileExpiredApply(context.Context, RecoveryUpdate) error
	ExpiredSendingCommands(context.Context, int) ([]uuid.UUID, error)
	LockExpiredSendingCommand(context.Context, uuid.UUID) (RecoveryCommand, error)
	ReconcileExpiredSending(context.Context, RecoveryUpdate) (bool, error)
	StaleSentCommands(context.Context, time.Duration, int) ([]uuid.UUID, error)
	LockStaleSentCommand(context.Context, uuid.UUID, time.Duration) (RecoveryCommand, error)
	ReconcileMissingResult(context.Context, RecoveryUpdate) (bool, error)
	StaleReconciliations(context.Context, int, time.Duration, int) ([]RecoveryCommand, error)
	ContinueReconciliation(context.Context, RecoveryUpdate) (bool, error)
	DeleteExpiredLeases(context.Context) error
}
