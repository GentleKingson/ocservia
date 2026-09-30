// Package store is the storage boundary for independent history retention.
package store

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Command struct {
	ID, NodeID uuid.UUID
	Envelope   []byte
}
type Store interface {
	// Candidates locks outbox rows first, matching dispatch/result lock order.
	Candidates(context.Context, value.Timestamp) ([]uuid.UUID, error)
	LockCommand(context.Context, uuid.UUID, value.Timestamp) (Command, error)
	Compact(context.Context, Command, []byte, []byte, value.Timestamp) error
	CompactRetired(context.Context, value.Timestamp) error
}
type Provider interface{ HistoryRetentionStore() Store }
