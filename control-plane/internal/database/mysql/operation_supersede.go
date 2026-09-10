package mysql

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func (s operationStore) SupersedePending(ctx context.Context, node uuid.UUID, payload string, at value.Timestamp) error {
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE o.locked_by IS NULL AND EXISTS(
		SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.node_id=? AND BINARY c.payload_type=? AND c.state='queued'
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)) ORDER BY o.id FOR UPDATE`, UUIDBytes(node), payload)
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
		err := s.QueryRow(ctx, `SELECT operation_id FROM commands WHERE id=? AND state='queued'
			AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=commands.id) FOR UPDATE`, UUIDBytes(command)).Scan(&operation)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE commands SET state='superseded',updated_at=? WHERE id=?`, at, UUIDBytes(command)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=?,completed_at=? WHERE id=? AND state='queued'`, at, at, UUIDBytes(operation)); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=?,last_error='superseded by newer intent' WHERE command_id=?`, at, UUIDBytes(command)); err != nil {
			return err
		}
		event, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'superseded',?)`, UUIDBytes(event), UUIDBytes(operation), at); err != nil {
			return err
		}
	}
	return nil
}
