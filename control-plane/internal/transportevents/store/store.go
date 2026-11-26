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

type IngressTrust struct {
	WorkspaceID               uuid.UUID
	NodeStatus, EndpointState string
}

type TransportEvent struct {
	ID, NodeID        uuid.UUID
	Type, Traceparent string
	Payload           []byte
	At                time.Time
}

type Quarantine struct {
	EventID, NodeID          uuid.UUID
	Type                     int32
	PayloadHash              []byte
	ReasonCode, ReasonDetail string
	At                       time.Time
}

type IngressStore interface {
	SaveBusiness(context.Context) error
	RollbackBusiness(context.Context) error
	ReleaseBusiness(context.Context) error
	LockTrust(context.Context, uuid.UUID, []byte) (IngressTrust, error)
	LockTrustShared(context.Context, uuid.UUID, []byte) (IngressTrust, error)
	InsertEvent(context.Context, TransportEvent) (bool, error)
	NodeStatus(context.Context, uuid.UUID, string, time.Time) error
	SimulationOutcome(context.Context, uuid.UUID, string, string, bool, time.Time) error
	InsertQuarantine(context.Context, Quarantine) (bool, error)
	Quarantine(context.Context, uuid.UUID) (Quarantine, error)
	QuarantineAlert(context.Context, uuid.UUID, uuid.UUID, Quarantine) error
	AdvanceCursor(context.Context, uuid.UUID, time.Time) error
	LastEventID(context.Context) (uuid.UUID, error)
}

func Ingress(tx database.Tx) IngressStore {
	return database.Must[interface{ TransportIngressStore() IngressStore }](tx).TransportIngressStore()
}

type ResultCommand struct {
	OperationID           uuid.UUID
	Compacted             bool
	Envelope              []byte
	State                 string
	CreatedAt, ExpiresAt  time.Time
	ReceiptRecovery       bool
	DispatchInFlight      bool
	AttemptID, LeaseToken uuid.UUID
}

type Result struct {
	EventID, CommandID, IdempotencyKey uuid.UUID
	PayloadHash, Bytes                 []byte
	HashVersion                        int16
	State                              string
	ErrorCode                          *string
	AcceptedAt                         *time.Time
	CompletedAt                        time.Time
	Replayed                           bool
	VerificationStatus                 string
	FailureReason, KeyID               *string
	ReceiptHash, Proof                 []byte
	EffectSequence                     *int64
}

type ResultOperation struct {
	ID, WorkspaceID    uuid.UUID
	RequestID, TraceID string
}

type ConfigOutcome struct {
	OperationID, NodeID, AlertID, WorkspaceID uuid.UUID
	State                                     string
	FailureCode                               *string
	Revision                                  uint64
	CandidateHash                             []byte
	At                                        time.Time
}

type CSROutcome struct {
	CertificateID, OperationID            uuid.UUID
	State                                 string
	DER, PublicHash, ReceiptHash, CSRHash []byte
	KeyID                                 *string
	At                                    time.Time
	VerifiedAt                            *time.Time
}

type RevokeOutcome struct {
	CertificateID, NodeID uuid.UUID
	State, Reason         string
	RevokedAt             *time.Time
	At                    time.Time
}

type ArtifactOutcome struct {
	ArtifactID, OperationID uuid.UUID
	State                   string
	Digest                  []byte
	Size                    *int64
	At                      time.Time
}

type ResultStore interface {
	LoadCommand(context.Context, uuid.UUID, uuid.UUID) (ResultCommand, error)
	Receipt(context.Context, string, uint64) (uuid.UUID, []byte, error)
	InsertResult(context.Context, Result) error
	Workspace(context.Context, uuid.UUID) (uuid.UUID, error)
	Alert(context.Context, uuid.UUID, uuid.UUID, string, string, time.Time) error
	UpdateCommand(context.Context, uuid.UUID, string, time.Time) (bool, error)
	UpdateOperation(context.Context, uuid.UUID, uuid.UUID, string, bool, time.Time) (ResultOperation, error)
	CompleteOutbox(context.Context, uuid.UUID, time.Time) error
	CloseDispatch(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) error
	ConfigOutcome(context.Context, ConfigOutcome) (bool, error)
	CSROutcome(context.Context, CSROutcome) error
	RevokeOutcome(context.Context, RevokeOutcome) error
	ArtifactOutcome(context.Context, ArtifactOutcome) error
	UpgradeOutcome(context.Context, uuid.UUID, string, *time.Time, time.Time) error
	AppendEvent(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error
	ScheduleRecovery(context.Context, uuid.UUID, []byte, time.Time, time.Time, bool) error
}

func Results(tx database.Tx) ResultStore {
	return database.Must[interface{ CommandResultStore() ResultStore }](tx).CommandResultStore()
}
