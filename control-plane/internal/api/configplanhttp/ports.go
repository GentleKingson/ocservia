package configplanhttp

import (
	"context"
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

type Plans interface {
	Create(context.Context, configplan.CreateRequest) (configplan.Plan, bool, error)
	Get(context.Context, uuid.UUID) (configplan.Plan, error)
	Apply(context.Context, configplan.ApplyRequest) (operations.Operation, bool, error)
}

// RequestInfo contains only values obtained from the authenticated request.
// Workspace ownership remains with the parent's resource and Secret guards.
type RequestInfo struct {
	ActorID         string
	ActorIdentityID uuid.UUID
	ActorSessionID  uuid.UUID
	RequestID       string
	Traceparent     string
}

// SecretUse checks one reference's ownership and secret.use permission. It
// writes the existing denial/unavailable response before returning false.
type SecretUse func(http.ResponseWriter, *http.Request, uuid.UUID) bool
