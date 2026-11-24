package store

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

var ErrIdempotencyConflict = errors.New("idempotency key was reused with different input")

type Operation struct {
	ID                     string     `json:"id"`
	State                  string     `json:"state"`
	NodeID                 *string    `json:"node_id,omitempty"`
	CommandID              *string    `json:"command_id,omitempty"`
	ConfigApplyState       string     `json:"config_apply_state,omitempty"`
	ConfigApplyFailureCode string     `json:"config_apply_failure_code,omitempty"`
	AgentUpgradeState      string     `json:"agent_upgrade_state,omitempty"`
	AgentUpgradeTarget     string     `json:"agent_upgrade_target_version,omitempty"`
	Version                int64      `json:"version"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

type Event struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operation_id"`
	State       string    `json:"state"`
	OccurredAt  time.Time `json:"occurred_at"`
	Sequence    int64     `json:"-"`
}

type Summary struct {
	Active  int64 `json:"active"`
	Unknown int64 `json:"unknown"`
}

type QueuedIntent struct {
	ID, WorkspaceID, NodeID, CommandID uuid.UUID
	RequestID, TraceID, IdempotencyKey string
	RequestHash                        []byte
	ExpiresAt, CreatedAt               time.Time
}

type QueuedCommand struct {
	ID, OperationID, WorkspaceID, NodeID, OutboxID, EventID uuid.UUID
	PayloadType, IdempotencyKey, Traceparent                string
	ResourceType, ResourceKey                               *string
	Envelope                                                []byte
	ExpectedVersion                                         int64
	ExpiresAt, CreatedAt, AvailableAt                       time.Time
}

type NodeState struct {
	WorkspaceID           uuid.UUID
	Version               int64
	AuthorizationRevision uint64
	Status                string
}

type RolloutClaim struct {
	State, NodeState string
	DispatchLease    *time.Time
}

type AgentObservation struct {
	Architecture, AgentVersion, OSRelease string
	LastHeartbeatAt                       *time.Time
}

type RolloutObservation struct {
	NodeID                                        uuid.UUID
	Status, Architecture, AgentVersion, OSRelease string
	LastHeartbeatAt                               *time.Time
	CapabilityOK, UpgradeActive                   bool
}

type PendingUpgrade struct {
	ID, WorkspaceID, NodeID                  uuid.UUID
	TargetVersion, Architecture, FromVersion string
	PackageSHA256                            []byte
	CreatedAt                                time.Time
}

type ScheduledUpgrade struct {
	OperationID, WorkspaceID, NodeID, CommandID                                                                 uuid.UUID
	TargetVersion, FromVersion, State, OperationState, NodeStatus, ObservedVersion, DurableState, DurableDetail string
	ScheduledAt, CreatedAt                                                                                      time.Time
	ObservedAt, LastHeartbeatAt                                                                                 *time.Time
}

type UpgradeTransition struct {
	OperationID, CommandID              uuid.UUID
	State, OperationState, CommandState string
	GenericTerminal                     bool
	At                                  time.Time
}

type Store interface {
	DispatchStore
	RecoveryStore
	ReapingStore
	Rollout(context.Context, uuid.UUID, bool) (Rollout, error)
	Rollouts(context.Context, uuid.UUID, int) ([]Rollout, error)
	RolloutNodes(context.Context, uuid.UUID) ([]RolloutNode, error)
	RolloutWorkspace(context.Context, uuid.UUID) (uuid.UUID, error)
	FindRollout(context.Context, uuid.UUID, string) (uuid.UUID, []byte, error)
	LockRolloutIntent(context.Context, uuid.UUID, string) error
	InsertRollout(context.Context, PendingRollout) error
	InsertRolloutNode(context.Context, uuid.UUID, RolloutNode, time.Time) error
	ResumeRollout(context.Context, uuid.UUID, int, time.Time) (int64, error)
	ActiveRollouts(context.Context, int) ([]uuid.UUID, error)
	SetRolloutState(context.Context, uuid.UUID, string, string, time.Time) error
	SetRolloutBatch(context.Context, uuid.UUID, int, time.Time) error
	TerminalRolloutNodes(context.Context, uuid.UUID) ([]RolloutTerminalNode, error)
	SetRolloutNodeOutcome(context.Context, uuid.UUID, uuid.UUID, string, string, time.Time) (bool, error)
	RolloutNodeVersion(context.Context, uuid.UUID) (int64, error)
	ClaimRolloutNode(context.Context, uuid.UUID, RolloutNode, time.Time, time.Time) (bool, error)
	SkipRolloutNode(context.Context, uuid.UUID, RolloutNode, string, time.Time) (bool, error)
	AttachRolloutOperation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, time.Time) error
	LockRolloutNode(context.Context, uuid.UUID, uuid.UUID) (RolloutNode, error)
	ReleaseRolloutClaim(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	LockNode(context.Context, uuid.UUID) (NodeState, error)
	HasCapability(context.Context, uuid.UUID, string) (bool, error)
	AttestationReady(context.Context, uuid.UUID) (bool, error)
	HasSession(context.Context, uuid.UUID, string, string) (bool, error)
	HasIPBan(context.Context, uuid.UUID, string) (bool, error)
	LockRolloutClaim(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (RolloutClaim, error)
	UpgradeNode(context.Context, uuid.UUID) (uuid.UUID, string, string, error)
	LockAgentObservation(context.Context, uuid.UUID) (AgentObservation, error)
	LockUpgradeCapability(context.Context, uuid.UUID) (bool, error)
	HasActiveUpgrade(context.Context, uuid.UUID) (bool, error)
	RolloutObservation(context.Context, uuid.UUID, uuid.UUID) (RolloutObservation, error)
	InsertUpgrade(context.Context, PendingUpgrade) error
	PendingUpgrades(context.Context, int) ([]ScheduledUpgrade, error)
	// ErrNotFound from either transition requires rollback before treating
	// the lost state fence as a no-op.
	UpgradeTerminal(context.Context, UpgradeTransition) error
	UpgradeProgress(context.Context, UpgradeTransition) error
	InsertIntent(context.Context, QueuedIntent) error
	EnqueueCommand(context.Context, QueuedCommand) error
	ExpireQueued(context.Context) error
	SupersedePending(context.Context, uuid.UUID, string, time.Time) error
	Get(context.Context, uuid.UUID) (Operation, error)
	OperationState(context.Context, uuid.UUID) (string, error)
	ListInWorkspace(context.Context, uuid.UUID, uuid.UUID, int) ([]Operation, error)
	SummaryInWorkspace(context.Context, uuid.UUID) (Summary, error)
	FindIdempotent(context.Context, uuid.UUID, string) (Operation, []byte, error)
	Events(context.Context, uuid.UUID, uuid.UUID, int) ([]Event, error)
	EventSequence(context.Context, uuid.UUID, uuid.UUID) (int64, error)
}

func FromTransaction(tx database.Tx) (Store, error) {
	p, ok := tx.(interface{ OperationStore() Store })
	if !ok {
		return nil, database.ErrUnsupported
	}
	return p.OperationStore(), nil
}
