package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Event struct {
	ID          string          `json:"id"`
	NodeID      string          `json:"node_id"`
	Type        string          `json:"type"`
	Traceparent string          `json:"traceparent"`
	OccurredAt  value.Timestamp `json:"occurred_at"`
	Sequence    int64           `json:"-"`
}

type Job struct {
	OperationID, NodeID uuid.UUID
	Envelope            []byte
	Traceparent         string
}

type Simulation struct {
	WorkspaceID, NodeID, OperationID, CommandID uuid.UUID
	Endpoint                                    []byte
	Envelope                                    []byte
	RequestID, TraceID, Traceparent             string
	At, ExpiresAt                               value.Timestamp
}

type GapNode struct {
	ID          uuid.UUID
	Traceparent string
}

type LocalStore interface {
	EnsureWorkspace(context.Context, uuid.UUID, string, value.Timestamp) (uuid.UUID, error)
	InsertSimulation(context.Context, Simulation) error
	Events(context.Context, uuid.UUID, uuid.UUID, int, bool) ([]Event, error)
	EventSequence(context.Context, uuid.UUID, uuid.UUID) (int64, error)
	GapNodes(context.Context, string) ([]GapNode, error)
	InvalidateCursor(context.Context) error
	UnknownDispatched(context.Context) error
	DisconnectNode(context.Context, GapNode, uuid.UUID, value.Timestamp) error
	ClaimJobs(context.Context, int) ([]Job, error)
	ExpireJobs(context.Context) error
	MarkDispatchStarted(context.Context, uuid.UUID) (bool, error)
	MarkDispatchError(context.Context, uuid.UUID, string) error
}

func Local(tx database.Tx) (LocalStore, error) {
	p, ok := tx.(interface{ LocalSliceStore() LocalStore })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.LocalSliceStore(), nil
}
