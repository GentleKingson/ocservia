package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Dispatch struct {
	AttemptID, CommandID, OperationID, OutboxID, NodeID, LeaseToken uuid.UUID
	Envelope                                                        []byte
	Traceparent                                                     string
}

type DispatchStatus struct {
	LeaseValid, AttemptValid, OwnsOutboxLock, ResultAfterAttempt bool
	CommandState                                                 string
	Envelope                                                     []byte
}

type QueueMetrics struct {
	Unpublished          int64   `json:"outbox_unpublished_total"`
	OldestAge            float64 `json:"outbox_oldest_age_seconds"`
	Queued               int64   `json:"command_queue_depth"`
	Unknown              int64   `json:"command_unknown_total"`
	ConfigRollbacks      int64   `json:"config_rollback_total"`
	ConfigFailedCritical int64   `json:"config_failed_critical_total"`
}

type DispatchStore interface {
	DispatchCandidates(context.Context, int, int, time.Time) ([]Dispatch, error)
	ClaimDispatch(context.Context, Dispatch, uuid.UUID, time.Time, time.Time) (bool, error)
	LockDispatchOutbox(context.Context, Dispatch) error
	DispatchStatus(context.Context, Dispatch) (DispatchStatus, error)
	CompletedDispatch(context.Context, Dispatch) (DispatchStatus, error)
	PublishDispatch(context.Context, Dispatch, time.Time) error
	RecordDispatched(context.Context, Dispatch, bool, time.Time) error
	CloseDispatch(context.Context, Dispatch, time.Time) error
	FailDispatch(context.Context, Dispatch, string, time.Time) error
	QueueMetrics(ctx context.Context, workspaceID uuid.UUID, all bool) (QueueMetrics, error)
}
