package store

import (
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
