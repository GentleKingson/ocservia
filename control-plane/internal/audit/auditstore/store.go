// Package auditstore is the storage boundary shared by audit and SQL adapters.
package auditstore

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type VpnAuditCursor struct {
	At   time.Time
	Node uuid.UUID
	Key  string
	Kind string
}
type VpnAuditFilter struct {
	After             *VpnAuditCursor
	Kind, Destination string
	Workspace         uuid.UUID
	Node              *uuid.UUID
	Username          string
	Since, Until      *time.Time
	Limit             int
}

type Store interface {
	VpnAuditRecords(context.Context, VpnAuditFilter) (database.Rows, error)
	Lock(context.Context, uuid.UUID) error
	Clock(context.Context) (time.Time, error)
	Previous(context.Context, uuid.UUID) ([]byte, error)
	Append(context.Context, []any) error
	Events(context.Context, uuid.UUID) (database.Rows, error)
	EventsFrom(context.Context, uuid.UUID, value.Timestamp, uuid.UUID) (database.Rows, error)
	LatestEvent(context.Context, uuid.UUID) (uuid.UUID, []byte, error)
	RecentEvents(context.Context, uuid.UUID, int) (database.Rows, error)
	LatestCheckpoint(context.Context, uuid.UUID) (uuid.UUID, []byte, []byte, value.Timestamp, int16, error)
	AddCheckpoint(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte, []byte) error
	Workspaces(context.Context) (database.Rows, error)
	CheckpointProof(context.Context, uuid.UUID, uuid.UUID) (hash, signature []byte, err error)
	ExpiredDetails(context.Context, value.Timestamp) (database.Rows, error)
	CompactDetails(context.Context, uuid.UUID, []byte, string, []byte, value.Timestamp) error
	CompactSecurity(context.Context, value.Timestamp) error
}

type Provider interface{ AuditStore() Store }

func From(s database.Store) (Store, error) {
	p, ok := s.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.AuditStore(), nil
}
