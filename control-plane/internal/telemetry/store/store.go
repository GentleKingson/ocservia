// Package store defines persistence operations for telemetry ingestion, read models, observed state, and history.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrInvalidJSON = errors.New("telemetry JSON is not PostgreSQL-compatible")

type Node struct {
	ID                                                                       uuid.UUID
	Name, Status                                                             string
	Version                                                                  int64
	ConfigRevision                                                           int64
	ObservedAt, Heartbeat                                                    *time.Time
	BootID, InstanceID, AgentVersion, OcservVersion, OSRelease, Architecture string
	Ocserv, System, Path                                                     json.RawMessage
	Security, Health, Aggregate, Raw                                         uint64
	Sessions                                                                 int
	UpgradeEligible                                                          bool
}

type ReadSession struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	ClientIP    string    `json:"client_ip"`
	ConnectedAt time.Time `json:"connected_at"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
}

type IPBan struct {
	IP               string  `json:"ip"`
	SecondsRemaining *uint64 `json:"seconds_remaining,omitempty"`
}

type Snapshot struct {
	ObservedAt                                           time.Time
	BootID                                               string
	AgentInstance                                        uuid.UUID
	AgentVersion, OcservVersion, OSRelease, Architecture string
	Ocserv, System, Path                                 json.RawMessage
	Security, Health, Aggregate, Raw                     uint64
}

type Session struct {
	ID, Username, ClientIP, VPNIP string
	ConnectedAt                   time.Time
	BytesIn, BytesOut             int64
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

type Sample struct {
	SampledAt time.Time
	Metric    string
	Value     float64
}

type Point struct {
	At      time.Time `json:"at"`
	Metric  string    `json:"metric"`
	Count   int64     `json:"count"`
	Minimum float64   `json:"minimum"`
	Maximum float64   `json:"maximum"`
	Average float64   `json:"average"`
}

type Group struct {
	Name        string
	Members     []string
	Revision    int64
	Fingerprint []byte
	ObservedAt  time.Time
}

type SecurityEvent struct {
	ID             uuid.UUID
	ObservedAt     time.Time
	Severity, Type string
	Detail         json.RawMessage
}

type Store interface {
	Nodes(context.Context, uuid.UUID, uuid.UUID, int) ([]Node, error)
	Node(context.Context, uuid.UUID) (Node, error)
	Sessions(context.Context, uuid.UUID, string, int) ([]ReadSession, error)
	IPBans(context.Context, uuid.UUID, int) ([]IPBan, error)

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

	InsertHistory(context.Context, uuid.UUID, uuid.UUID, []Sample) error
	History(context.Context, uuid.UUID, string, string, time.Time) ([]Point, error)
	MaintainHistory(context.Context, time.Time) error

	ReplaceGroups(context.Context, uuid.UUID, []Group) error
	InsertSecurity(context.Context, uuid.UUID, []SecurityEvent) error
}

type BatchMaintainer interface {
	MaintainBatch(context.Context, time.Time) (done bool, err error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	if provider, ok := tx.(interface{ TelemetryStore() Store }); ok {
		return provider.TelemetryStore(), nil
	}
	return nil, database.ErrUnsupported
}

// Maintain commits each batch together with the caller's leadership fence.
// beforeFirst stays atomic with the first batch; already committed progress survives recovery.
func Maintain(ctx context.Context, backend database.Backend, now time.Time, beforeFirst, fence func(context.Context, database.Tx) error) error {
	first := true
	for {
		done := true
		err := database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
			if first && beforeFirst != nil {
				if err := beforeFirst(ctx, tx); err != nil {
					return err
				}
			}
			store, err := FromTransaction(tx)
			if err != nil {
				return err
			}
			if batch, ok := store.(BatchMaintainer); ok {
				done, err = batch.MaintainBatch(ctx, now)
			} else {
				err = store.MaintainHistory(ctx, now)
			}
			if err != nil {
				return err
			}
			return fence(ctx, tx)
		})
		if err != nil || done {
			return err
		}
		first = false
	}
}
