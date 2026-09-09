package rbacstore

import (
	"context"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
	"time"
)

type Binding struct {
	ID, IdentityID, WorkspaceID, ResourceID, ActorID, ApprovalID uuid.UUID
	Role, ResourceType                                           string
	At                                                           time.Time
}
type Store interface {
	LockManagement(context.Context) error
	Roles(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) (database.Rows, error)
	WorkspaceRoles(context.Context, uuid.UUID, uuid.UUID) (database.Rows, error)
	Node(context.Context, uuid.UUID) database.Row
	Operation(context.Context, uuid.UUID) database.Row
	Workspace(context.Context, uuid.UUID) database.Row
	AuthorizedWorkspaces(context.Context, uuid.UUID, bool) (database.Rows, error)
	Insert(context.Context, Binding) error
	CompleteBootstrap(context.Context, uuid.UUID, uuid.UUID, time.Time) error
}
type Provider interface{ RBACStore() Store }

func From(s database.Store) (Store, error) {
	p, ok := s.(Provider)
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.RBACStore(), nil
}
