package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Event struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	Type        string    `json:"type"`
	Traceparent string    `json:"traceparent"`
	OccurredAt  time.Time `json:"occurred_at"`
	Sequence    int64     `json:"-"`
}

type SimulationJob struct {
	OperationID, NodeID uuid.UUID
	Envelope            []byte
	Traceparent         string
}

type Simulation struct {
	WorkspaceID, NodeID, OperationID, CommandID uuid.UUID
	Endpoint                                    []byte
	Envelope                                    []byte
	RequestID, TraceID, Traceparent             string
	At, ExpiresAt                               time.Time
}

type GapNode struct {
	ID          uuid.UUID
	Traceparent string
}

type Store interface {
	EnsureWorkspace(context.Context, uuid.UUID, string, time.Time) (uuid.UUID, error)
	InsertSimulation(context.Context, Simulation) error
	Events(context.Context, uuid.UUID, uuid.UUID, int, bool) ([]Event, error)
	EventSequence(context.Context, uuid.UUID, uuid.UUID) (int64, error)
	GapNodes(context.Context, string) ([]GapNode, error)
	InvalidateCursor(context.Context) error
	UnknownDispatched(context.Context) error
	DisconnectNode(context.Context, GapNode, uuid.UUID, time.Time) error
	ClaimSimulationJobs(context.Context, int) ([]SimulationJob, error)
	ExpireSimulationJobs(context.Context) error
	MarkSimulationDispatchStarted(context.Context, uuid.UUID) (bool, error)
	MarkSimulationDispatchError(context.Context, uuid.UUID, string) error
}

func FromTransaction(tx database.Tx) Store {
	return database.Must[interface{ TransportEventStore() Store }](tx).TransportEventStore()
}
