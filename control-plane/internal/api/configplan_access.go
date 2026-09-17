package api

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/google/uuid"
)

// configPlanLookup is the read-only view used by authorization and approvals.
type configPlanLookup interface {
	Get(context.Context, uuid.UUID) (configplan.Plan, error)
	Resource(context.Context, uuid.UUID) (workspaceID, nodeID uuid.UUID, err error)
}

var _ configPlanLookup = (*configplan.Service)(nil)
