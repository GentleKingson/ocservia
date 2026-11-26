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
	OwnerActive                                                              *bool
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
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	ClientIP    string    `json:"client_ip"`
	VPNIP       string    `json:"vpn_ip,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
}

type User struct {
	Username string `json:"username"`
	Enabled  bool   `json:"enabled"`
	Revision uint64 `json:"revision"`
}

type Upgrade struct {
	OperationID                  uuid.UUID
	State, TargetVersion, Detail string
	CompletedAt                  time.Time
	Proof, PackageSHA256         []byte
}

type Sample struct {
	SampledAt time.Time `json:"sampled_at"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
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
	Name       string
	Members    []string
	Revision   int64
	ObservedAt time.Time
}

type SecurityEvent struct {
	ID         uuid.UUID       `json:"id"`
	ObservedAt time.Time       `json:"observed_at"`
	Severity   string          `json:"severity"`
	Type       string          `json:"type"`
	Detail     json.RawMessage `json:"detail"`
}

type Store interface {
	Nodes(context.Context, uuid.UUID, uuid.UUID, int) ([]Node, error)
	Node(context.Context, uuid.UUID) (Node, error)
	Sessions(context.Context, uuid.UUID, string, int) ([]ReadSession, error)
	IPBans(context.Context, uuid.UUID, int) ([]IPBan, error)

	LockNode(context.Context, uuid.UUID) error
	InsertObservation(context.Context, uuid.UUID, uuid.UUID, uint64, string, int, Snapshot, []SecurityEvent) (inserted, updated bool, err error)
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
	EnsureUpcomingPartitions(context.Context) error

	ReplaceGroups(context.Context, uuid.UUID, []Group) error
}

func FromTransaction(tx database.Tx) Store {
	return database.Must[interface{ TelemetryStore() Store }](tx).TelemetryStore()
}
