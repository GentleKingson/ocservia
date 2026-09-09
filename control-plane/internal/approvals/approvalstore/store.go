package approvalstore

import (
	"context"
	"github.com/google/uuid"
)

type Store interface {
	ConsumeBound(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, uuid.UUID, []byte) (bool, error)
}
type Provider interface{ ApprovalStore() Store }
