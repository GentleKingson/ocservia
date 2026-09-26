package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func (b *Backend) CheckReadiness(ctx context.Context) error {
	// Check current core reads and permissions without scanning migration history.
	var workspace, node, operation bool
	return b.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE false), EXISTS(SELECT 1 FROM nodes WHERE false), EXISTS(SELECT 1 FROM operations WHERE false)`).Scan(&workspace, &node, &operation)
}

func (b *Backend) PoolStats() database.PoolStats {
	s := b.pool.Stats()
	return database.PoolStats{Acquired: int64(s.InUse), Idle: int64(s.Idle), Total: int64(s.OpenConnections)}
}

var _ database.Diagnostics = (*Backend)(nil)
