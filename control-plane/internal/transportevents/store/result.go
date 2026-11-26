package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

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
