// Package store defines the policy and batch persistence boundary.
package store

import (
	"context"
	"time"

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

type Batch struct {
	ID, WorkspaceID                                                uuid.UUID
	ActorIdentityID, ActorSessionID, ApprovalID                    *uuid.UUID
	State, ActorID, Reason, RequestID, Traceparent, IdempotencyKey string
	Hash                                                           []byte
	CreatedAt, UpdatedAt                                           value.Timestamp
}

type BatchItem struct {
	BatchID                            uuid.UUID
	Index                              int
	NodeID                             uuid.UUID
	Username, Action, State, ErrorType string
	ExpectedVersion                    int64
	ChildOperationID                   *uuid.UUID
	At                                 value.Timestamp
}

type ApprovalItem struct {
	NodeID           uuid.UUID
	Username, Action string
	ExpectedVersion  int64
}

type Candidate struct {
	NodeID                                      uuid.UUID
	Username, Cause                             string
	PolicyVersion, UserVersion, EnforcedVersion int64
	PeriodStart                                 value.Timestamp
	Enabled                                     bool
}

type Metrics struct {
	PolicyPendingTotal    int64 `json:"policy_pending_total"`
	ActiveBatchItemTotal  int64 `json:"active_batch_item_total"`
	StaleBatchClaimTotal  int64 `json:"stale_batch_claim_total"`
	UnknownBatchItemTotal int64 `json:"unknown_batch_item_total"`
}

type Store interface {
	LockUser(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Mutation(context.Context, uuid.UUID, string) (Mutation, error)
	LockPolicy(context.Context, uuid.UUID, string) (int64, error)
	PutPolicy(context.Context, Policy) error
	InsertMutation(context.Context, Mutation) error
	Policy(context.Context, uuid.UUID, string, value.Timestamp) (PolicyState, error)
	BatchApproval(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (uuid.UUID, []byte, error)
	ApprovalItems(context.Context, uuid.UUID) ([]ApprovalItem, error)
	BatchByKey(context.Context, uuid.UUID, string) (uuid.UUID, []byte, error)
	InsertBatch(context.Context, Batch) error
	UserExists(context.Context, uuid.UUID, string, uuid.UUID) (bool, error)
	InsertBatchItem(context.Context, BatchItem) error
	Batch(context.Context, uuid.UUID) (Batch, error)
	BatchItems(context.Context, uuid.UUID) ([]BatchItem, error)
	Metrics(context.Context, uuid.UUID) (Metrics, error)
	ActiveOperations(context.Context) (int, error)
	ResetCandidates(context.Context, value.Timestamp, value.Timestamp, int) ([]Candidate, error)
	EnforcementCandidates(context.Context, value.Timestamp, value.Timestamp, int) ([]Candidate, error)
	EnsureEnforcement(context.Context, Candidate, value.Timestamp) error
	DeleteEnforcement(context.Context, Candidate, bool) error
	CompleteEnforcement(context.Context, Candidate, uuid.UUID, int64) error
	FindUserOperation(context.Context, uuid.UUID, string, string, string) (uuid.UUID, error)
	AcquireLease(context.Context, string, uuid.UUID, time.Duration) (bool, error)
	ClaimBatchItems(context.Context, uuid.UUID, int) ([]BatchItem, error)
	ReleaseBatchClaims(context.Context, uuid.UUID) error
	FinishBatchItem(context.Context, BatchItem, uuid.UUID, *uuid.UUID, string) error
	RefreshBatchItems(context.Context, int) error
	RefreshBatches(context.Context, int) error
}

func From(tx database.Tx) (Store, error) {
	if p, ok := tx.(interface{ UserOperationsStore() Store }); ok {
		return p.UserOperationsStore(), nil
	}
	return nil, database.ErrUnsupported
}
