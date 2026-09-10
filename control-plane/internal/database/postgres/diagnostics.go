package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
)

func (b *Backend) Ping(ctx context.Context) error { return classify(b.pool.Ping(ctx)) }

func (b *Backend) PoolStats() database.PoolStats {
	s := b.pool.Stat()
	return database.PoolStats{Acquired: int64(s.AcquiredConns()), Idle: int64(s.IdleConns()), Total: int64(s.TotalConns())}
}

func (b *Backend) ControllerSchema(ctx context.Context, expected int64) (int64, error) {
	compatibility, err := migrations.ValidateControllerSchema(ctx, b.pool, expected)
	return compatibility.CurrentSchema, err
}

var _ database.Diagnostics = (*Backend)(nil)
