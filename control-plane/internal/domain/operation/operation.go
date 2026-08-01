package operation

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type State string

const (
	StateDraft     State = "draft"
	StateQueued    State = "queued"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateUnknown   State = "unknown"
)

type Operation struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	NodeID      *uuid.UUID
	State       State
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (o Operation) Validate() error {
	if o.ID == uuid.Nil || o.WorkspaceID == uuid.Nil {
		return errors.New("operation identifiers are required")
	}
	if o.Version < 1 {
		return errors.New("operation version must be positive")
	}
	switch o.State {
	case StateDraft, StateQueued, StateSucceeded, StateFailed, StateUnknown:
		return nil
	default:
		return errors.New("invalid operation state")
	}
}
