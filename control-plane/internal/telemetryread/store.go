// Package telemetryread defines lossless node and session read models.
package telemetryread

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Node struct {
	ID                                                                       uuid.UUID
	Name, Status                                                             string
	Version                                                                  int64
	ObservedAt, Heartbeat                                                    value.Timestamp
	BootID, InstanceID, AgentVersion, OcservVersion, OSRelease, Architecture string
	Ocserv, System, Path                                                     value.JSONB
	Security, Health, Aggregate, Raw                                         uint64
	Sessions                                                                 int
}

type Session struct {
	ID          string          `json:"id"`
	Username    string          `json:"username"`
	ClientIP    string          `json:"client_ip"`
	ConnectedAt value.Timestamp `json:"connected_at"`
	BytesIn     int64           `json:"bytes_in"`
	BytesOut    int64           `json:"bytes_out"`
}

type IPBan struct {
	IP               string  `json:"ip"`
	SecondsRemaining *uint64 `json:"seconds_remaining,omitempty"`
}

type Store interface {
	Nodes(context.Context, uuid.UUID, uuid.UUID, int) ([]Node, error)
	Node(context.Context, uuid.UUID) (Node, error)
	UpgradeEligibility(context.Context, uuid.UUID) (bool, error)
	Sessions(context.Context, uuid.UUID, string, int) ([]Session, error)
	IPBans(context.Context, uuid.UUID, int) ([]IPBan, error)
}

type Provider interface{ TelemetryReadStore() Store }

func From(tx database.Tx) (Store, error) {
	if provider, ok := tx.(Provider); ok {
		return provider.TelemetryReadStore(), nil
	}
	return nil, database.ErrUnsupported
}
