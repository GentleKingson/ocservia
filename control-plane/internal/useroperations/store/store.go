// Package store defines the policy and batch persistence boundary.
package store

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type Policy struct {
	NodeID                                uuid.UUID
	Username, QuotaPeriod, QuotaDirection string
	QuotaBytes, Version                   int64
	ExpiresAt                             *time.Time
	CreatedAt, UpdatedAt                  time.Time
}

type PolicyState struct {
	Policy
	PeriodStart                      time.Time
	ObservedAt                       *time.Time
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
	At                       time.Time
}

type Candidate struct {
	NodeID                                      uuid.UUID
	Username, Cause                             string
	PolicyVersion, UserVersion, EnforcedVersion int64
	PeriodStart                                 time.Time
	Enabled                                     bool
}

type Store interface {
	LockUser(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Mutation(context.Context, uuid.UUID, string) (Mutation, error)
	LockPolicy(context.Context, uuid.UUID, string) (int64, error)
	PutPolicy(context.Context, Policy) error
	InsertMutation(context.Context, Mutation) error
	Policy(context.Context, uuid.UUID, string, time.Time) (PolicyState, error)
	ActiveOperations(context.Context) (int, error)
	ResetCandidates(context.Context, time.Time, time.Time, int) ([]Candidate, error)
	EnforcementCandidates(context.Context, time.Time, time.Time, int) ([]Candidate, error)
	EnsureEnforcement(context.Context, Candidate, time.Time) error
	DeleteEnforcement(context.Context, Candidate, bool) error
	CompleteEnforcement(context.Context, Candidate, uuid.UUID, int64) error
	FindUserOperation(context.Context, uuid.UUID, string, string, string) (uuid.UUID, error)
}

func From(tx database.Tx) Store {
	return database.Must[interface{ UserOperationsStore() Store }](tx).UserOperationsStore()
}
