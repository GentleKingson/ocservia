package store

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type Operation struct {
	ID                     string           `json:"id"`
	State                  string           `json:"state"`
	NodeID                 *string          `json:"node_id,omitempty"`
	CommandID              *string          `json:"command_id,omitempty"`
	ConfigApplyState       string           `json:"config_apply_state,omitempty"`
	ConfigApplyFailureCode string           `json:"config_apply_failure_code,omitempty"`
	AgentUpgradeState      string           `json:"agent_upgrade_state,omitempty"`
	AgentUpgradeTarget     string           `json:"agent_upgrade_target_version,omitempty"`
	Version                int64            `json:"version"`
	CreatedAt              value.Timestamp  `json:"created_at"`
	UpdatedAt              value.Timestamp  `json:"updated_at"`
	ExpiresAt              *value.Timestamp `json:"expires_at,omitempty"`
}

type Event struct {
	ID          string          `json:"id"`
	OperationID string          `json:"operation_id"`
	State       string          `json:"state"`
	OccurredAt  value.Timestamp `json:"occurred_at"`
	Sequence    int64           `json:"-"`
}

type Summary struct {
	Active  int64 `json:"active"`
	Unknown int64 `json:"unknown"`
}

type QueuedIntent struct {
	ID, WorkspaceID, NodeID, CommandID uuid.UUID
	RequestID, TraceID, IdempotencyKey string
	RequestHash                        []byte
	ExpiresAt, CreatedAt               value.Timestamp
}

type QueuedCommand struct {
	ID, OperationID, WorkspaceID, NodeID, OutboxID, EventID uuid.UUID
	PayloadType, IdempotencyKey, Traceparent                string
	ResourceType, ResourceKey                               *string
	Envelope                                                []byte
	ExpectedVersion                                         int64
	ExpiresAt, CreatedAt, AvailableAt                       value.Timestamp
}

type NodeState struct {
	WorkspaceID           uuid.UUID
	Version               int64
	AuthorizationRevision uint64
	Status                string
}

type RolloutClaim struct {
	State, NodeState string
	DispatchLease    value.Timestamp
}

type AgentObservation struct {
	Architecture, AgentVersion string
	LastHeartbeatAt            value.Timestamp
}

type RolloutObservation struct {
	NodeID                             uuid.UUID
	Status, Architecture, AgentVersion string
	LastHeartbeatAt                    value.Timestamp
	CapabilityOK, UpgradeActive        bool
}

type PendingUpgrade struct {
	ID, WorkspaceID, NodeID                  uuid.UUID
	ApprovalID                               *uuid.UUID
	TargetVersion, Architecture, FromVersion string
	PackageSHA256                            []byte
	CreatedAt                                value.Timestamp
}

type ScheduledUpgrade struct {
	OperationID, WorkspaceID, NodeID, CommandID                                                                 uuid.UUID
	TargetVersion, FromVersion, State, OperationState, NodeStatus, ObservedVersion, DurableState, DurableDetail string
	ScheduledAt, CreatedAt, ObservedAt, LastHeartbeatAt                                                         value.Timestamp
}

type UpgradeTransition struct {
	OperationID, CommandID              uuid.UUID
	State, OperationState, CommandState string
	GenericTerminal                     bool
	At                                  value.Timestamp
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
	RolloutApproval(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, []byte, value.Timestamp) (uuid.UUID, error)
	InsertRollout(context.Context, PendingRollout) error
	InsertRolloutNode(context.Context, uuid.UUID, RolloutNode, value.Timestamp) error
	ResumeRollout(context.Context, uuid.UUID, int, value.Timestamp) (int64, error)
	ActiveRollouts(context.Context, int) ([]uuid.UUID, error)
	SetRolloutState(context.Context, uuid.UUID, string, string, value.Timestamp) error
	SetRolloutBatch(context.Context, uuid.UUID, int, value.Timestamp) error
	TerminalRolloutNodes(context.Context, uuid.UUID) ([]RolloutTerminalNode, error)
	SetRolloutNodeOutcome(context.Context, uuid.UUID, uuid.UUID, string, string, value.Timestamp) (bool, error)
	RolloutNodeVersion(context.Context, uuid.UUID) (int64, error)
	ClaimRolloutNode(context.Context, uuid.UUID, RolloutNode, value.Timestamp, value.Timestamp) (bool, error)
	SkipRolloutNode(context.Context, uuid.UUID, RolloutNode, string, value.Timestamp) (bool, error)
	AttachRolloutOperation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, value.Timestamp) error
	LockRolloutNode(context.Context, uuid.UUID, uuid.UUID) (RolloutNode, error)
	ReleaseRolloutClaim(context.Context, uuid.UUID, uuid.UUID, value.Timestamp) error
	LockNode(context.Context, uuid.UUID) (NodeState, error)
	HasCapability(context.Context, uuid.UUID, string) (bool, error)
	AttestationReady(context.Context, uuid.UUID) (bool, error)
	HasSession(context.Context, uuid.UUID, string, string) (bool, error)
	HasIPBan(context.Context, uuid.UUID, string) (bool, error)
	LockRolloutClaim(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (RolloutClaim, error)
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
	NotifyOutbox(context.Context, uuid.UUID) error
	ExpireQueued(context.Context) error
	SupersedePending(context.Context, uuid.UUID, string, value.Timestamp) error
	Get(context.Context, uuid.UUID) (Operation, error)
	ListInWorkspace(context.Context, uuid.UUID, uuid.UUID, int) ([]Operation, error)
	SummaryInWorkspace(context.Context, uuid.UUID) (Summary, error)
	FindIdempotent(context.Context, uuid.UUID, string, []byte) (Operation, bool, error)
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
