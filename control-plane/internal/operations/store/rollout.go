package store

import (
	"encoding/json"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Rollout struct {
	ID, WorkspaceID, ApprovalID, CreatedBy, ActorSession uuid.UUID
	TargetVersion, State, Reason, PauseCode              string
	BatchSize, CurrentBatch                              int
	StopOnFailure                                        bool
	RequestHash                                          []byte
	Excluded                                             []json.RawMessage
	CreatedAt, UpdatedAt                                 value.Timestamp
}

type RolloutNode struct {
	NodeID          uuid.UUID
	Ordinal, Batch  int
	State           string
	OperationID     uuid.UUID
	FromVersion     string
	FailureCode     string
	DispatchVersion int64
	DispatchAttempt int
	DispatchLease   value.Timestamp
}

type PendingRollout struct {
	Rollout
	IdempotencyKey string
}

type RolloutTerminalNode struct {
	NodeID         uuid.UUID
	Ordinal, Batch int
	OperationID    uuid.UUID
	Outcome        string
}
