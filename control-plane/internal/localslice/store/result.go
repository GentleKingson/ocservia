package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type ResultCommand struct {
	Envelope              []byte
	State                 string
	CreatedAt             value.Timestamp
	DispatchInFlight      bool
	AttemptID, LeaseToken uuid.UUID
}

type Result struct {
	EventID, CommandID, IdempotencyKey uuid.UUID
	PayloadHash, Bytes                 []byte
	HashVersion                        int16
	State                              string
	ErrorCode                          *string
	AcceptedAt, CompletedAt            value.Timestamp
	Replayed                           bool
	VerificationStatus                 string
	FailureReason, KeyID               *string
	EffectRecordID, ReceiptHash, Proof []byte
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
	At                                        value.Timestamp
}

type CSROutcome struct {
	CertificateID, OperationID                                         uuid.UUID
	State                                                              string
	DER, PublicHash, ReceiptHash, EffectRecordID, CSRHash, SubjectHash []byte
	KeyID                                                              *string
	At, VerifiedAt                                                     value.Timestamp
}

type RevokeOutcome struct {
	CertificateID, NodeID uuid.UUID
	State, Reason         string
	RevokedAt, At         value.Timestamp
}

type ArtifactOutcome struct {
	ArtifactID, OperationID uuid.UUID
	State                   string
	Digest                  []byte
	Size                    *int64
	At                      value.Timestamp
}

type ResultStore interface {
	LoadCommand(context.Context, uuid.UUID, uuid.UUID) (ResultCommand, error)
	Receipt(context.Context, string, []byte, uint64) (uuid.UUID, []byte, error)
	InsertResult(context.Context, Result) error
	Workspace(context.Context, uuid.UUID) (uuid.UUID, error)
	Alert(context.Context, uuid.UUID, uuid.UUID, string, string, value.Timestamp) error
	UpdateCommand(context.Context, uuid.UUID, string, value.Timestamp) (bool, error)
	UpdateOperation(context.Context, uuid.UUID, string, bool, value.Timestamp) (ResultOperation, error)
	CompleteOutbox(context.Context, uuid.UUID, value.Timestamp) error
	CloseDispatch(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) error
	ConfigOutcome(context.Context, ConfigOutcome) (bool, error)
	CSROutcome(context.Context, CSROutcome) error
	RevokeOutcome(context.Context, RevokeOutcome) error
	ArtifactOutcome(context.Context, ArtifactOutcome) error
	UpgradeOutcome(context.Context, uuid.UUID, string, value.Timestamp, value.Timestamp) error
	AppendEvent(context.Context, uuid.UUID, uuid.UUID, string, value.Timestamp) error
	ScheduleRecovery(context.Context, uuid.UUID, []byte, value.Timestamp, value.Timestamp) error
}

func Results(tx database.Tx) (ResultStore, error) {
	if p, ok := tx.(interface{ CommandResultStore() ResultStore }); ok {
		return p.CommandResultStore(), nil
	}
	return nil, database.ErrUnsupported
}
