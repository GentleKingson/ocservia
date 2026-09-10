package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) RecoveryClock(ctx context.Context) (at value.Timestamp, err error) {
	err = s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return
}

func reapingCommandIDs(rows database.Rows, err error) ([]uuid.UUID, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s operationStore) ExpiredApplyCommands(ctx context.Context) ([]operationstore.RecoveryCommand, error) {
	ids, err := reapingCommandIDs(s.Query(ctx, `SELECT c.id FROM outbox_events o JOIN commands c ON c.id=o.command_id
		JOIN config_apply_operations a ON a.operation_id=c.operation_id
		WHERE c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=now()
		ORDER BY c.id FOR UPDATE OF o SKIP LOCKED`))
	if err != nil {
		return nil, err
	}
	var result []operationstore.RecoveryCommand
	for _, id := range ids {
		v := operationstore.RecoveryCommand{CommandID: id}
		err := s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope FROM commands c JOIN config_apply_operations a ON a.operation_id=c.operation_id
			WHERE c.id=$1 AND c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=now() FOR UPDATE OF c`, id).Scan(&v.OperationID, &v.NodeID, &v.Envelope)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

const reapingSendingFrom = ` FROM commands c JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
	JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
	JOIN command_attempts a ON a.command_id=c.id AND a.outbox_event_id=o.id AND a.worker_id=l.worker_id AND a.attempt_number=o.attempts
	WHERE l.leased_until<=clock_timestamp() AND a.state='sending' AND a.finished_at IS NULL
	AND c.state IN ('queued','unknown') AND p.state IN ('queued','unknown')
	AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
	AND o.published_at IS NULL AND o.locked_by=l.worker_id`

func (s operationStore) ExpiredSendingCommands(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return reapingCommandIDs(s.Query(ctx, `SELECT c.id`+reapingSendingFrom+` ORDER BY l.leased_until,c.id LIMIT $1 FOR UPDATE OF o SKIP LOCKED`, limit))
}

func (s operationStore) LockExpiredSendingCommand(ctx context.Context, id uuid.UUID) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,a.id,c.state,p.state,o.attempts`+reapingSendingFrom+` AND c.id=$1 FOR UPDATE OF c,p`, id).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.AttemptID, &v.CommandState, &v.OperationState, &v.Attempts)
	return
}

const reapingSentFrom = ` FROM commands c JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
	JOIN LATERAL(SELECT state,finished_at,attempt_number FROM command_attempts WHERE command_id=c.id ORDER BY attempt_number DESC LIMIT 1) a ON true
	WHERE c.state IN ('dispatched','accepted','running') AND p.state IN ('dispatched','accepted','running','unknown')
	AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
	AND o.published_at IS NOT NULL AND o.locked_by IS NULL AND a.state='sent' AND a.attempt_number=o.attempts
	AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)`

func (s operationStore) StaleSentCommands(ctx context.Context, timeout time.Duration, limit int) ([]uuid.UUID, error) {
	return reapingCommandIDs(s.Query(ctx, `SELECT c.id`+reapingSentFrom+` AND a.finished_at<=clock_timestamp()-$1::interval ORDER BY a.finished_at,c.id LIMIT $2 FOR UPDATE OF o SKIP LOCKED`, timeout.String(), limit))
}

func (s operationStore) LockStaleSentCommand(ctx context.Context, id uuid.UUID, timeout time.Duration) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,o.attempts`+reapingSentFrom+` AND c.id=$1 AND a.finished_at<=clock_timestamp()-$2::interval FOR UPDATE OF c,p`, id, timeout.String()).Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.Attempts)
	return
}

func (s operationStore) StaleReconciliations(ctx context.Context, maxAttempts int, timeout time.Duration, limit int) ([]operationstore.RecoveryCommand, error) {
	rows, err := s.Query(ctx, `SELECT c.id,c.operation_id,c.node_id,c.envelope,o.id FROM commands c
		JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
		JOIN LATERAL(SELECT state,finished_at FROM command_attempts WHERE command_id=c.id ORDER BY attempt_number DESC LIMIT 1) a ON true
		WHERE c.state='unknown' AND p.state='unknown' AND o.published_at IS NOT NULL AND o.locked_by IS NULL
		AND o.attempts<$1 AND a.state='sent' AND a.finished_at<=clock_timestamp()-$2::interval AND c.expires_at>clock_timestamp()
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)
		ORDER BY a.finished_at,c.id LIMIT $3 FOR UPDATE OF o SKIP LOCKED`, maxAttempts, timeout.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []operationstore.RecoveryCommand
	for rows.Next() {
		var v operationstore.RecoveryCommand
		if err := rows.Scan(&v.CommandID, &v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s operationStore) DeleteExpiredLeases(ctx context.Context) error {
	_, err := s.Exec(ctx, `DELETE FROM node_command_leases l WHERE l.leased_until<=now()
		AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=l.command_id AND a.state='sending')`)
	return err
}

func (s operationStore) ReconcileExpiredApply(ctx context.Context, v operationstore.RecoveryUpdate) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state IN ('dispatched','accepted','running')`, v.Command.CommandID, v.Envelope, v.ExpiresAt, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=$2,updated_at=$3 WHERE id=$1 AND state IN ('dispatched','accepted','running')`, v.Command.OperationID, v.ExpiresAt, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=$2 WHERE operation_id=$1 AND state IN ('dispatched','accepted','running')`, v.Command.OperationID, v.At); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='apply outcome missing; reconciliation required' WHERE command_id=$1`, v.Command.CommandID, v.Envelope, v.At); err != nil {
		return err
	}
	return s.recoveryEvent(ctx, v.Command.OperationID, v.At)
}

func (s operationStore) recoveryEvent(ctx context.Context, operation uuid.UUID, at value.Timestamp) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'unknown',$3)`, id, operation, at)
	return err
}

func (s operationStore) ReconcileExpiredSending(ctx context.Context, v operationstore.RecoveryUpdate) (bool, error) {
	c := v.Command
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='unknown',finished_at=$2,error_code='lease_expired_outcome_unknown' WHERE id=$1 AND state='sending' AND finished_at IS NULL`, c.AttemptID, v.At)
	if err != nil || n != 1 {
		return false, err
	}
	if c.CommandState == "queued" {
		n, err = s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state='queued'`, c.CommandID, v.Envelope, v.ExpiresAt, v.At)
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, database.ErrNotFound
		}
		n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=$2,updated_at=$3,completed_at=NULL WHERE id=$1 AND state='queued'`, c.OperationID, v.ExpiresAt, v.At)
		if err != nil {
			return false, err
		}
		if n != 1 || c.OperationState != "queued" {
			return false, database.ErrNotFound
		}
		if err := s.recoveryEvent(ctx, c.OperationID, v.At); err != nil {
			return false, err
		}
	} else if v.Schedule {
		n, err = s.Exec(ctx, `UPDATE commands SET envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state='unknown'`, c.CommandID, v.Envelope, v.ExpiresAt, v.At)
		if err != nil {
			return false, err
		}
		if n != 1 || c.OperationState != "unknown" {
			return false, database.ErrNotFound
		}
	}
	if v.Schedule {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='dispatch outcome unknown; reconciliation required' WHERE id=$1 AND published_at IS NULL`, c.OutboxID, v.Envelope, v.At)
	} else {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET published_at=$2,locked_by=NULL,locked_until=NULL,last_error='reconciliation attempt limit reached after unknown dispatch outcome' WHERE id=$1 AND published_at IS NULL`, c.OutboxID, v.At)
	}
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, database.ErrNotFound
	}
	return true, nil
}

func (s operationStore) ReconcileMissingResult(ctx context.Context, v operationstore.RecoveryUpdate) (bool, error) {
	c := v.Command
	n, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state IN ('dispatched','accepted','running')`, c.CommandID, v.Envelope, v.ExpiresAt, v.At)
	if err != nil || n != 1 {
		return false, err
	}
	n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=$2,updated_at=$3,completed_at=NULL WHERE id=$1 AND state IN ('dispatched','accepted','running','unknown')`, c.OperationID, v.ExpiresAt, v.At)
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, database.ErrNotFound
	}
	if v.Schedule {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='command result missing; reconciliation required' WHERE id=$1 AND published_at IS NOT NULL AND locked_by IS NULL`, c.OutboxID, v.Envelope, v.At)
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, database.ErrNotFound
		}
	} else if _, err := s.Exec(ctx, `UPDATE outbox_events SET last_error='command result missing; reconciliation attempt limit reached' WHERE id=$1 AND published_at IS NOT NULL AND locked_by IS NULL`, c.OutboxID); err != nil {
		return false, err
	}
	return true, s.recoveryEvent(ctx, c.OperationID, v.At)
}

func (s operationStore) ContinueReconciliation(ctx context.Context, v operationstore.RecoveryUpdate) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,updated_at=$3 WHERE id=$1 AND state='unknown'`, v.Command.CommandID, v.Envelope, v.At)
	if err != nil || n != 1 {
		return false, err
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='reconciliation response missing; continuing reconcile-only delivery' WHERE id=$1 AND published_at IS NOT NULL AND locked_by IS NULL`, v.Command.OutboxID, v.Envelope, v.At)
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, database.ErrNotFound
	}
	return true, nil
}
