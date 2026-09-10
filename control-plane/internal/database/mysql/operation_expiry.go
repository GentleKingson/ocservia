package mysql

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

func (s operationStore) ExpireQueued(ctx context.Context) error {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	// Claim and result ingestion serialize on the durable outbox row. Recheck
	// the command and its lease after obtaining that lock, not from a stale
	// candidate snapshot. A lease is never cleared by ordinary TTL expiry.
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS(
		SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.state='queued' AND c.expires_at<=?
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id))
		ORDER BY o.id FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return err
	}
	var commands []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, command := range commands {
		var operation uuid.UUID
		err := s.QueryRow(ctx, `SELECT operation_id FROM commands WHERE id=? AND state='queued' AND expires_at<=?
			AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=commands.id) FOR UPDATE`, UUIDBytes(command), now).Scan(&operation)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE commands SET state='expired',updated_at=? WHERE id=?`, now, UUIDBytes(command)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=?,locked_by=NULL,locked_until=NULL,last_error='command expired before dispatch' WHERE command_id=?`, now, UUIDBytes(command)); err != nil {
			return err
		}
		tag, err := s.Exec(ctx, `UPDATE operations SET state='expired',version=version+1,updated_at=?,completed_at=? WHERE id=? AND state='queued'`, now, now, UUIDBytes(operation))
		if err != nil {
			return err
		}
		if tag == 0 {
			continue
		}
		event, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'expired',?)`, UUIDBytes(event), UUIDBytes(operation), now); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='expired',updated_at=? WHERE operation_id=? AND state='queued'`, now, UUIDBytes(operation)); err != nil {
			return err
		}
	}
	return nil
}
