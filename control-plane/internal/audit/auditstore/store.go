// Package auditstore is the storage boundary shared by audit and SQL adapters.
package auditstore

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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

type EventRow struct {
	ID, WorkspaceID              uuid.UUID
	At                           time.Time
	ActorType, ActorID           string
	Action, ResourceType         string
	ResourceID                   uuid.UUID
	RequestID, TraceID           string
	Result, Reason               string
	SessionID, NodeID, CommandID *uuid.UUID
	BeforeSummary, AfterSummary  []byte
	ErrorType                    string
	PreviousHash, EventHash      []byte
	AuthVersion                  int16
	EventKeyID                   *string
	EventMAC                     []byte
	CompactedAt                  *time.Time
	CompactionKeyID              *string
	CompactionMAC                []byte
}

type RecentEvent struct {
	ID                uuid.UUID  `json:"id"`
	OccurredAt        time.Time  `json:"occurred_at"`
	ActorType         string     `json:"actor_type"`
	ActorID           string     `json:"actor_id"`
	Action            string     `json:"action"`
	ResourceType      string     `json:"resource_type"`
	ResourceID        *uuid.UUID `json:"resource_id"`
	NodeID            *uuid.UUID `json:"node_id"`
	RequestID         string     `json:"request_id"`
	TraceID           *string    `json:"trace_id"`
	CommandID         *uuid.UUID `json:"command_id"`
	Result            string     `json:"result"`
	Reason            *string    `json:"reason"`
	ErrorType         *string    `json:"error_type"`
	PreviousEventHash string     `json:"previous_event_hash"`
	EventHash         string     `json:"event_hash"`
}

type VpnAuditRecord struct {
	NodeID          uuid.UUID  `json:"node_id"`
	SessionID       string     `json:"session_id"`
	Username        string     `json:"username"`
	ClientIP        string     `json:"client_ip"`
	VPNIP           *string    `json:"vpn_ip,omitempty"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	BytesIn         *string    `json:"bytes_in,omitempty"`
	BytesOut        *string    `json:"bytes_out,omitempty"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	AbsentAt        *time.Time `json:"absent_at,omitempty"`
	EventType       string     `json:"event_type"`
	EventID         *uuid.UUID `json:"event_id,omitempty"`
	DestinationIP   *string    `json:"destination_ip,omitempty"`
	DestinationPort *int32     `json:"destination_port,omitempty"`
	Protocol        *string    `json:"protocol,omitempty"`
	FlowStartedAt   *time.Time `json:"flow_started_at,omitempty"`
}

type Store interface {
	VpnAuditRecords(context.Context, VpnAuditFilter) ([]VpnAuditRecord, error)
	Lock(context.Context, uuid.UUID) error
	Clock(context.Context) (time.Time, error)
	Previous(context.Context, uuid.UUID) ([]byte, error)
	Append(context.Context, EventRow) error
	Events(context.Context, uuid.UUID, func(EventRow) error) error
	EventsFrom(context.Context, uuid.UUID, time.Time, uuid.UUID, func(EventRow) error) error
	LatestEvent(context.Context, uuid.UUID) (uuid.UUID, []byte, error)
	RecentEvents(context.Context, uuid.UUID, int) ([]RecentEvent, error)
	LatestCheckpoint(context.Context, uuid.UUID) (uuid.UUID, []byte, []byte, time.Time, int16, error)
	AddCheckpoint(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte, []byte) error
	Workspaces(context.Context) ([]uuid.UUID, error)
	ExpiredDetails(context.Context, time.Time) ([]EventRow, error)
	CompactDetails(context.Context, uuid.UUID, []byte, string, []byte, time.Time) error
	CompactSecurity(context.Context, time.Time) error
}

func Of(v any) Store {
	return database.Must[interface{ AuditStore() Store }](v).AuditStore()
}
