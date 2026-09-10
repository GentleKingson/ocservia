package database

import "context"

type PoolStats struct {
	Acquired, Idle, Total int64
}

// Diagnostics backs Controller readiness and the existing pool metrics.
type Diagnostics interface {
	Ping(context.Context) error
	PoolStats() PoolStats
	ControllerSchema(context.Context, int64) (int64, error)
}
