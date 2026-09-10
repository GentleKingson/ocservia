package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func (s operationStore) SupersedePending(ctx context.Context, node uuid.UUID, payload string, at value.Timestamp) error {
	rows, err := s.Query(ctx, `UPDATE commands AS command SET state='superseded',updated_at=$3 FROM outbox_events AS outbox WHERE command.node_id=$1 AND command.payload_type=$2 AND command.state='queued' AND outbox.command_id=command.id AND outbox.locked_by IS NULL AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id) RETURNING command.operation_id,command.id`, node, payload, at)
	if err != nil {
		return err
	}
	type old struct{ operation, command uuid.UUID }
	var olds []old
	for rows.Next() {
		var v old
		if err := rows.Scan(&v.operation, &v.command); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range olds {
		event, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1 AND state='queued'`, v.operation, at); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=$2,last_error='superseded by newer intent' WHERE command_id=$1`, v.command, at); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'superseded',$3)`, event, v.operation, at); err != nil {
			return err
		}
	}
	return nil
}
