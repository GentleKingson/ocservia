package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type RecoveryCommand struct {
	CommandID, OperationID, NodeID, OutboxID, AttemptID uuid.UUID
	Envelope                                            []byte
	CommandState, OperationState                        string
	Attempts                                            int
}

type RecoveryUpdate struct {
	Command       RecoveryCommand
	Envelope      []byte
	ExpiresAt, At value.Timestamp
	Schedule      bool
}

type ReapingStore interface {
	RecoveryClock(context.Context) (value.Timestamp, error)
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
