package mysql

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

func (b *Backend) Ping(ctx context.Context) error { return safeError(b.pool.PingContext(ctx)) }

func (b *Backend) PoolStats() database.PoolStats {
	s := b.pool.Stats()
	return database.PoolStats{Acquired: int64(s.InUse), Idle: int64(s.Idle), Total: int64(s.OpenConnections)}
}

// Runtime checks the complete immutable receipt history and compatibility,
// like PostgreSQL readiness. Physical trigger/routine inspection remains the
// owner's ValidateSchema operation; runtime must not receive DDL privileges.
func (b *Backend) ControllerSchema(ctx context.Context, expected int64) (current int64, result error) {
	chain, err := loadRevisionChain(b.engine)
	if err != nil {
		return 0, err
	}
	latest := chain[len(chain)-1]
	if expected < int64(latest.MinimumControllerSchema) || expected > int64(latest.ControllerSchema) {
		return 0, ErrSchema
	}
	conn, name, err := migrationConnection(ctx, b)
	if err != nil {
		return 0, err
	}
	defer func() { result = errors.Join(result, releaseMigrationConnection(conn, name)) }()
	root, parent, err := b.rootOn(ctx, conn)
	if err != nil {
		return 0, err
	}
	if err := b.validateBaselineReceipts(ctx, conn, root, parent); err != nil {
		return 0, err
	}
	if n, err := revisionTables(ctx, conn, chain[0].revision); err != nil {
		return 0, err
	} else if n != 2 {
		return 0, ErrSchema
	}
	states, _, err := readRevisionHistory(ctx, conn, chain, parent)
	if err != nil {
		return 0, err
	}
	for _, state := range states {
		if state != "verified" {
			return 0, ErrDirty
		}
	}
	if len(states) != len(chain) {
		return 0, ErrSchema
	}
	return int64(latest.ControllerSchema), nil
}

var _ database.Diagnostics = (*Backend)(nil)
