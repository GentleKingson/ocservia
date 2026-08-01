package operation

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type State string

const (
	StateDraft          State = "draft"
	StateQueued         State = "queued"
	StateDispatched     State = "dispatched"
	StateAccepted       State = "accepted"
	StateRunning        State = "running"
	StateSucceeded      State = "succeeded"
	StateFailed         State = "failed"
	StateUnknown        State = "unknown"
	StateExpired        State = "expired"
	StateRolledBack     State = "rolled_back"
	StateOfflinePending State = "offline_pending"
	StateDrifted        State = "drifted"
	StateSuperseded     State = "superseded"
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
	case StateDraft, StateQueued, StateDispatched, StateAccepted, StateRunning,
		StateSucceeded, StateFailed, StateUnknown, StateExpired, StateRolledBack,
		StateOfflinePending, StateDrifted, StateSuperseded:
		return nil
	default:
		return errors.New("invalid operation state")
	}
}
