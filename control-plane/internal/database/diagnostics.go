package database

import "context"

type PoolStats struct {
	Acquired, Idle, Total int64
}

// Diagnostics backs Controller readiness and the existing pool metrics.
type Diagnostics interface {
	CheckReadiness(context.Context) error
	PoolStats() PoolStats
}
