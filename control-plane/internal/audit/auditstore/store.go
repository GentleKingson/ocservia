// Package auditstore is the storage boundary shared by audit and SQL adapters.
package auditstore

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Store interface {
	Lock(context.Context, uuid.UUID) error
	Clock(context.Context) (time.Time, error)
	Previous(context.Context, uuid.UUID) ([]byte, error)
	Append(context.Context, []any) error
	Events(context.Context, uuid.UUID) (database.Rows, error)
	RecentEvents(context.Context, uuid.UUID, int) (database.Rows, error)
	Checkpoints(context.Context, uuid.UUID) (database.Rows, error)
	AddCheckpoint(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte, []byte) error
	Workspaces(context.Context) (database.Rows, error)
	CheckpointHash(context.Context, uuid.UUID, uuid.UUID) ([]byte, error)
}

type Provider interface{ AuditStore() Store }

// LegacyPreflight is used only while migrating checkpointed PostgreSQL history.
type LegacyPreflight interface {
	LockLegacy(context.Context) error
	LegacyEvents(context.Context, uuid.UUID) (database.Rows, error)
}

func From(s database.Store) (Store, error) {
	p, ok := s.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.AuditStore(), nil
}
