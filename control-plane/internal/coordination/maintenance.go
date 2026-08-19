package coordination

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RecordMaintenanceCompletion writes the G6-only durable completion marker
// after a scheduler maintenance body succeeds. The database recorder checks
// and locks the same exact live term, and CommitFenced repeats the session
// assertion immediately before commit.
func RecordMaintenanceCompletion(ctx context.Context, pool *pgxpool.Pool, session *Session) error {
	if session == nil {
		return errors.New("coordination: scheduler maintenance completion requires a session")
	}
	identity := session.Identity()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("coordination: begin scheduler maintenance completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx,
		`SELECT public.g6_record_scheduler_maintenance($1,$2,$3)`,
		identity.InstanceID, identity.Incarnation, session.Epoch()); err != nil {
		return fmt.Errorf("coordination: record scheduler maintenance completion: %w", err)
	}
	if err := CommitFenced(ctx, tx, session); err != nil {
		return fmt.Errorf("coordination: commit scheduler maintenance completion: %w", err)
	}
	return nil
}
