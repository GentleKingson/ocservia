package postgres

import (
	"context"

	"github.com/google/uuid"
)

func (s operationStore) ExpireQueued(ctx context.Context) error {
	// A leased command has an uncertain dispatch outcome. Only reconciliation
	// may resolve it, even when its execution deadline has already elapsed.
	rows, err := s.Query(ctx, `WITH expired AS (
		UPDATE commands SET state='expired',updated_at=now()
		WHERE state='queued' AND expires_at<=now()
		AND NOT EXISTS(SELECT 1 FROM node_command_leases lease WHERE lease.command_id=commands.id)
		RETURNING id,operation_id
	), stopped AS (
		UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error='command expired before dispatch'
		FROM expired WHERE outbox_events.command_id=expired.id RETURNING expired.operation_id
	) UPDATE operations SET state='expired',version=version+1,updated_at=now(),completed_at=now()
	FROM stopped WHERE operations.id=stopped.operation_id AND operations.state='queued' RETURNING operations.id`)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		event, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'expired',now())`, event, id); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='expired',updated_at=now() WHERE operation_id=$1 AND state='queued'`, id); err != nil {
			return err
		}
	}
	return nil
}
