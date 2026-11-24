package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Rollout struct {
	ID, WorkspaceID, CreatedBy, ActorSession uuid.UUID
	TargetVersion, State, Reason, PauseCode  string
	BatchSize, CurrentBatch                  int
	StopOnFailure                            bool
	RequestHash                              []byte
	Excluded                                 []json.RawMessage
	CreatedAt, UpdatedAt                     time.Time
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
	DispatchLease   *time.Time
}

type PendingRollout struct {
	ID, WorkspaceID, CreatedBy, ActorSession uuid.UUID
	TargetVersion, Reason, IdempotencyKey    string
	BatchSize                                int
	RequestHash                              []byte
	Exclusions                               json.RawMessage
	CreatedAt                                time.Time
}

type RolloutTerminalNode struct {
	NodeID         uuid.UUID
	Ordinal, Batch int
	OperationID    uuid.UUID
	Outcome        string
}
