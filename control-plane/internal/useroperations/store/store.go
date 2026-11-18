// Package store defines the policy and batch persistence boundary.
package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Policy struct {
	NodeID                                uuid.UUID
	Username, QuotaPeriod, QuotaDirection string
	QuotaBytes, Version                   int64
	ExpiresAt, CreatedAt, UpdatedAt       value.Timestamp
}

type PolicyState struct {
	Policy
	PeriodStart, ObservedAt          value.Timestamp
	ObservedRXBytes, ObservedTXBytes int64
	NodeStatus                       string
	DesiredEnabled                   bool
	DesiredRevision                  int64
	ObservedEnabled                  *bool
	ObservedRevision                 *int64
	OperationState                   *string
}

type Mutation struct {
	ID, WorkspaceID, NodeID  uuid.UUID
	Username, IdempotencyKey string
	Hash                     []byte
	Version                  int64
	At                       value.Timestamp
}

type Candidate struct {
	NodeID                                      uuid.UUID
	Username, Cause                             string
	PolicyVersion, UserVersion, EnforcedVersion int64
	PeriodStart                                 value.Timestamp
	Enabled                                     bool
}

type Store interface {
	LockUser(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Mutation(context.Context, uuid.UUID, string) (Mutation, error)
	LockPolicy(context.Context, uuid.UUID, string) (int64, error)
	PutPolicy(context.Context, Policy) error
	InsertMutation(context.Context, Mutation) error
	Policy(context.Context, uuid.UUID, string, value.Timestamp) (PolicyState, error)
	ActiveOperations(context.Context) (int, error)
	ResetCandidates(context.Context, value.Timestamp, value.Timestamp, int) ([]Candidate, error)
	EnforcementCandidates(context.Context, value.Timestamp, value.Timestamp, int) ([]Candidate, error)
	EnsureEnforcement(context.Context, Candidate, value.Timestamp) error
	DeleteEnforcement(context.Context, Candidate, bool) error
	CompleteEnforcement(context.Context, Candidate, uuid.UUID, int64) error
	FindUserOperation(context.Context, uuid.UUID, string, string, string) (uuid.UUID, error)
}

func From(tx database.Tx) (Store, error) {
	if p, ok := tx.(interface{ UserOperationsStore() Store }); ok {
		return p.UserOperationsStore(), nil
	}
	return nil, database.ErrUnsupported
}
