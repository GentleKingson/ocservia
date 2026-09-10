package approvalstore

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"time"
)

type Store interface {
	Insert(context.Context, []any) error
	AddAuthority(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) error
	AddBatchItem(context.Context, uuid.UUID, int, uuid.UUID, string, string, int64) error
	Get(context.Context, uuid.UUID, bool) database.Row
	Authorized(context.Context, uuid.UUID, uuid.UUID) (bool, error)
	Approve(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error
	ValidBound(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, uuid.UUID, []byte, bool) (bool, error)
	AuthorityResources(context.Context, uuid.UUID) (database.Rows, error)
	ConsumeBound(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, uuid.UUID, []byte) (bool, error)
}
type Provider interface{ ApprovalStore() Store }
