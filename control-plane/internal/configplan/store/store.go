package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Node struct {
	WorkspaceID   uuid.UUID
	Version       int64
	OcservVersion string
}

type Plan struct {
	ID, WorkspaceID, NodeID, OperationID   uuid.UUID
	TemplateName, State, CandidateRedacted string
	ExpectedRevision                       int64
	CandidateHash, Result                  []byte
	Warnings                               []byte
	ExpiresAt, CreatedAt                   time.Time
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
	ExpiresAt             time.Time
	State                 string
}

type PendingPlan struct {
	ID, WorkspaceID, NodeID         uuid.UUID
	CreatedBy                       *uuid.UUID
	TemplateName, CandidateRedacted string
	ExpectedRevision                uint64
	CandidateHash                   []byte
	Warnings                        []byte
	ExpiresAt, CreatedAt            time.Time
}

type PendingApply struct {
	ID, WorkspaceID, NodeID, PlanID   uuid.UUID
	ExpectedRevision, DesiredRevision uint64
	CandidateHash, PreviousHash       []byte
	CreatedAt                         time.Time
}

type Store interface {
	Node(context.Context, uuid.UUID) (Node, error)
	Capabilities(context.Context, uuid.UUID) ([]string, error)
	Get(context.Context, uuid.UUID) (Plan, error)
	// A committed retry retains its allocated revision and expected node version;
	// Operations still compares the complete intent and owns approval consumption
	// and revision fencing.
	ApplyInput(context.Context, uuid.UUID, string) (ApplyInput, error)
	Resource(context.Context, uuid.UUID) (uuid.UUID, uuid.UUID, error)
	State(context.Context, uuid.UUID) (State, error)
	Proof(context.Context, uuid.UUID) (Proof, error)
	HasActiveApply(context.Context, uuid.UUID) (bool, error)
	InsertPlan(context.Context, PendingPlan) error
	InsertApply(context.Context, PendingApply) error
	AdvanceDesiredRevision(context.Context, uuid.UUID, uint64, time.Time) (bool, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ ConfigurationStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.ConfigurationStore(), nil
}
