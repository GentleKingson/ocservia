package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Node struct {
	WorkspaceID   uuid.UUID
	Version       int64
	OcservVersion string
}

type Plan struct {
	ID, WorkspaceID, NodeID, OperationID                   uuid.UUID
	TemplateName, State, CandidateRedacted, ApprovalStatus string
	ExpectedRevision                                       int64
	CandidateHash, Result                                  []byte
	Warnings                                               value.JSONB
	ExpiresAt, CreatedAt                                   value.Timestamp
	ApprovalID                                             *uuid.UUID
}

type ApplyInput struct {
	NodeVersion, DesiredRevision int64
	Envelope                     []byte
}

type State struct {
	Revision, DesiredRevision int64
	Locked                    bool
	OcservVersion             string
}

type Proof struct {
	WorkspaceID, NodeID   uuid.UUID
	ExpectedRevision      int64
	CandidateHash, Result []byte
	ExpiresAt             value.Timestamp
	State                 string
}

type PendingPlan struct {
	ID, WorkspaceID, NodeID         uuid.UUID
	CreatedBy                       *uuid.UUID
	TemplateName, CandidateRedacted string
	ExpectedRevision                uint64
	CandidateHash                   []byte
	Warnings                        value.JSONB
	ExpiresAt, CreatedAt            value.Timestamp
}

type PendingApply struct {
	ID, WorkspaceID, NodeID, PlanID, ApprovalID uuid.UUID
	ExpectedRevision, DesiredRevision           uint64
	CandidateHash, PreviousHash                 []byte
	CreatedAt                                   value.Timestamp
}

type Store interface {
	Node(context.Context, uuid.UUID) (Node, error)
	Capabilities(context.Context, uuid.UUID) ([]string, error)
	Get(context.Context, uuid.UUID) (Plan, error)
	ApplyInput(context.Context, uuid.UUID) (ApplyInput, error)
	Resource(context.Context, uuid.UUID) (uuid.UUID, uuid.UUID, error)
	State(context.Context, uuid.UUID) (State, error)
	Proof(context.Context, uuid.UUID) (Proof, error)
	HasActiveApply(context.Context, uuid.UUID) (bool, error)
	InsertPlan(context.Context, PendingPlan) error
	InsertApply(context.Context, PendingApply) error
	AdvanceDesiredRevision(context.Context, uuid.UUID, uint64, value.Timestamp) (bool, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ ConfigurationStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ConfigurationStore(), nil
}
