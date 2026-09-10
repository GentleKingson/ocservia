// Package telemetrywrite defines persistence operations for ingest and maintenance transactions.
package telemetrywrite

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrInvalidJSON = errors.New("telemetry JSON is not PostgreSQL-compatible")

type Snapshot struct {
	ObservedAt                                           time.Time
	BootID                                               string
	AgentInstance                                        uuid.UUID
	AgentVersion, OcservVersion, OSRelease, Architecture string
	Ocserv, System, Path                                 json.RawMessage
	Security, Health, Aggregate, Raw                     uint64
}
type Session struct {
	ID, Username, ClientIP string
	ConnectedAt            time.Time
	BytesIn, BytesOut      int64
}
type IPBan struct {
	IP               string
	SecondsRemaining *uint64
}
type User struct {
	Username    string
	Enabled     bool
	Revision    uint64
	Fingerprint []byte
}
type Upgrade struct {
	OperationID                  uuid.UUID
	State, TargetVersion, Detail string
	CompletedAt                  time.Time
	Proof, PackageSHA256         []byte
}

type Store interface {
	LockNode(context.Context, uuid.UUID) error
	ValidateJSON(context.Context, []json.RawMessage) error
	InsertBatch(context.Context, uuid.UUID, uuid.UUID, uint64, string, time.Time, int) (bool, error)
	UpsertSnapshot(context.Context, uuid.UUID, Snapshot) (bool, error)
	ReplaceSessions(context.Context, uuid.UUID, time.Time, []Session) error
	ReplaceIPBans(context.Context, uuid.UUID, time.Time, []IPBan) error
	ReplaceUsers(context.Context, uuid.UUID, time.Time, []User) error
	Activate(context.Context, uuid.UUID, time.Time) error
	LockOfflineCandidates(context.Context, time.Time) ([]uuid.UUID, error)
	MarkOffline(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error
	InsertUpgrade(context.Context, uuid.UUID, Upgrade) error
}
type Provider interface{ TelemetryWriteStore() Store }

func From(tx database.Tx) (Store, error) {
	if p, ok := tx.(Provider); ok {
		return p.TelemetryWriteStore(), nil
	}
	return nil, database.ErrUnsupported
}
