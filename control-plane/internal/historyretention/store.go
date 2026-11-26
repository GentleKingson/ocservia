package historyretention

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Command struct {
	ID, NodeID uuid.UUID
	Envelope   []byte
}
type Store interface {
	// Candidates locks outbox rows first, matching dispatch/result lock order.
	Candidates(context.Context, time.Time) ([]uuid.UUID, error)
	LockCommand(context.Context, uuid.UUID, time.Time) (Command, error)
	Compact(context.Context, Command, []byte, time.Time) error
	CompactRetired(context.Context, time.Time) error
}
