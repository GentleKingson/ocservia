package useroperationshttp

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

type Operations interface {
	GetPolicy(context.Context, uuid.UUID, string) (useroperations.Policy, error)
	SetPolicy(context.Context, useroperations.PolicyRequest) (useroperations.Policy, bool, error)
	CreateBatch(context.Context, useroperations.BatchRequest) (useroperations.Batch, bool, error)
	GetBatch(context.Context, uuid.UUID) (useroperations.Batch, error)
	Metrics(context.Context, uuid.UUID) (useroperations.Metrics, error)
}

type Authorizer interface {
	Node(context.Context, uuid.UUID) (rbac.Resource, error)
	Authorize(context.Context, uuid.UUID, string, rbac.Resource, bool) error
}

type RequestInfo struct {
	// Principal and WorkspaceID come from the parent's authorized context.
	Principal   auth.Principal
	WorkspaceID uuid.UUID
	ActorID     string
	RequestID   string
	Traceparent string
	// ApprovalID is only a parsed client reference. The domain transaction
	// validates its binding and consumes it, not the HTTP adapter.
	ApprovalID uuid.UUID
}
