package nodehttp

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryread"
	"github.com/google/uuid"
)

// Reader is the complete read capability consumed by this HTTP module.
type Reader interface {
	ListNodesInWorkspace(context.Context, uuid.UUID, uuid.UUID, int) ([]telemetry.Node, bool, error)
	GetNode(context.Context, uuid.UUID) (telemetry.Node, error)
	ListSessions(context.Context, uuid.UUID, string, int) ([]telemetryread.Session, bool, error)
	ListIPBans(context.Context, uuid.UUID, int) ([]telemetry.IPBan, error)
	HistoryFrom(context.Context, uuid.UUID, string, string, value.Timestamp) ([]telemetry.HistoryPoint, error)
}
