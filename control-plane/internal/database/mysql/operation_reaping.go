package mysql

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
	err = s.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))`).Scan(&at)
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
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return nil, err
	}
	ids, err := reapingCommandIDs(s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS(
		SELECT 1 FROM commands c JOIN config_apply_operations a ON a.operation_id=c.operation_id
		WHERE c.id=o.command_id AND c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=?)
		ORDER BY o.command_id FOR UPDATE SKIP LOCKED`, at))
	if err != nil {
		return nil, err
	}
	var result []operationstore.RecoveryCommand
	for _, id := range ids {
		v := operationstore.RecoveryCommand{CommandID: id}
		err := s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope FROM commands c JOIN config_apply_operations a ON a.operation_id=c.operation_id
			WHERE c.id=? AND c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=? FOR UPDATE`, UUIDBytes(id), at).Scan(&v.OperationID, &v.NodeID, &v.Envelope)
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

const reapingSendingPredicate = `l.leased_until<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))
	AND a.state='sending' AND a.finished_at IS NULL AND c.state IN ('queued','unknown') AND p.state IN ('queued','unknown')
	AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
	AND o.published_at IS NULL AND o.locked_by=l.worker_id`
const reapingSendingJoins = ` JOIN operations p ON p.id=c.operation_id
	JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
	JOIN command_attempts a ON a.command_id=c.id AND a.outbox_event_id=o.id AND a.worker_id=l.worker_id AND a.attempt_number=o.attempts`

func (s operationStore) ExpiredSendingCommands(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return reapingCommandIDs(s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS(SELECT 1 FROM commands c`+reapingSendingJoins+`
		WHERE c.id=o.command_id AND `+reapingSendingPredicate+`)
		ORDER BY (SELECT l.leased_until FROM node_command_leases l WHERE l.command_id=o.command_id),o.command_id LIMIT ? FOR UPDATE SKIP LOCKED`, limit))
}

func (s operationStore) LockExpiredSendingCommand(ctx context.Context, id uuid.UUID) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,a.id,c.state,p.state,o.attempts
		FROM outbox_events o JOIN commands c ON c.id=o.command_id`+reapingSendingJoins+` WHERE c.id=? AND `+reapingSendingPredicate+` FOR UPDATE`, UUIDBytes(id)).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.AttemptID, &v.CommandState, &v.OperationState, &v.Attempts)
	return
}

const reapingSentJoins = ` JOIN operations p ON p.id=c.operation_id
	JOIN command_attempts a ON a.command_id=c.id AND a.attempt_number=(SELECT MAX(z.attempt_number) FROM command_attempts z WHERE z.command_id=c.id)`
const reapingSentPredicate = `c.state IN ('dispatched','accepted','running') AND p.state IN ('dispatched','accepted','running','unknown')
	AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
	AND o.published_at IS NOT NULL AND o.locked_by IS NULL AND a.state='sent' AND a.attempt_number=o.attempts
	AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)`

func (s operationStore) StaleSentCommands(ctx context.Context, timeout time.Duration, limit int) ([]uuid.UUID, error) {
	return reapingCommandIDs(s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS(SELECT 1 FROM commands c`+reapingSentJoins+`
		WHERE c.id=o.command_id AND `+reapingSentPredicate+` AND a.finished_at<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))-?)
		ORDER BY (SELECT a.finished_at FROM command_attempts a WHERE a.command_id=o.command_id ORDER BY a.attempt_number DESC LIMIT 1),o.command_id LIMIT ? FOR UPDATE SKIP LOCKED`, timeout.Microseconds(), limit))
}

func (s operationStore) LockStaleSentCommand(ctx context.Context, id uuid.UUID, timeout time.Duration) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,o.attempts FROM outbox_events o JOIN commands c ON c.id=o.command_id`+reapingSentJoins+`
		WHERE c.id=? AND `+reapingSentPredicate+` AND a.finished_at<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))-? FOR UPDATE`, UUIDBytes(id), timeout.Microseconds()).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.Attempts)
	return
}

const reapingContinuationPredicate = `c.state='unknown' AND p.state='unknown' AND o.published_at IS NOT NULL AND o.locked_by IS NULL
	AND o.attempts<? AND a.state='sent' AND a.finished_at<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))-?
	AND c.expires_at>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))
	AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)`

func (s operationStore) StaleReconciliations(ctx context.Context, maxAttempts int, timeout time.Duration, limit int) ([]operationstore.RecoveryCommand, error) {
	ids, err := reapingCommandIDs(s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE EXISTS(SELECT 1 FROM commands c`+reapingSentJoins+`
		WHERE c.id=o.command_id AND `+reapingContinuationPredicate+`)
		ORDER BY (SELECT a.finished_at FROM command_attempts a WHERE a.command_id=o.command_id ORDER BY a.attempt_number DESC LIMIT 1),o.command_id LIMIT ? FOR UPDATE SKIP LOCKED`, maxAttempts, timeout.Microseconds(), limit))
	if err != nil {
		return nil, err
	}
	var result []operationstore.RecoveryCommand
	for _, id := range ids {
		v := operationstore.RecoveryCommand{CommandID: id}
		err := s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id FROM outbox_events o JOIN commands c ON c.id=o.command_id`+reapingSentJoins+`
			WHERE c.id=? AND `+reapingContinuationPredicate+` FOR UPDATE`, UUIDBytes(id), maxAttempts, timeout.Microseconds()).Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID)
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

func (s operationStore) DeleteExpiredLeases(ctx context.Context) error {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `DELETE l FROM node_command_leases l WHERE l.leased_until<=? AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=l.command_id AND a.state='sending')`, at)
	return err
}

func (s operationStore) ReconcileExpiredApply(ctx context.Context, v operationstore.RecoveryUpdate) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=?,expires_at=?,updated_at=? WHERE id=? AND state IN ('dispatched','accepted','running')`, v.Envelope, v.ExpiresAt, v.At, UUIDBytes(v.Command.CommandID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=?,updated_at=? WHERE id=? AND state IN ('dispatched','accepted','running')`, v.ExpiresAt, v.At, UUIDBytes(v.Command.OperationID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=? WHERE operation_id=? AND state IN ('dispatched','accepted','running')`, v.At, UUIDBytes(v.Command.OperationID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error='apply outcome missing; reconciliation required' WHERE command_id=?`, v.Envelope, v.At, UUIDBytes(v.Command.CommandID)); err != nil {
		return err
	}
	return s.recoveryEvent(ctx, v.Command.OperationID, v.At)
}

func (s operationStore) recoveryEvent(ctx context.Context, operation uuid.UUID, at value.Timestamp) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'unknown',?)`, UUIDBytes(id), UUIDBytes(operation), at)
	return err
}

func (s operationStore) ReconcileExpiredSending(ctx context.Context, v operationstore.RecoveryUpdate) (bool, error) {
	c := v.Command
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='unknown',finished_at=?,error_code='lease_expired_outcome_unknown' WHERE id=? AND state='sending' AND finished_at IS NULL`, v.At, UUIDBytes(c.AttemptID))
	if err != nil || n != 1 {
		return false, err
	}
	if c.CommandState == "queued" {
		n, err = s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=?,expires_at=?,updated_at=? WHERE id=? AND state='queued'`, v.Envelope, v.ExpiresAt, v.At, UUIDBytes(c.CommandID))
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, database.ErrNotFound
		}
		n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=?,updated_at=?,completed_at=NULL WHERE id=? AND state='queued'`, v.ExpiresAt, v.At, UUIDBytes(c.OperationID))
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
		n, err = s.Exec(ctx, `UPDATE commands SET envelope=?,expires_at=?,updated_at=? WHERE id=? AND state='unknown'`, v.Envelope, v.ExpiresAt, v.At, UUIDBytes(c.CommandID))
		if err != nil {
			return false, err
		}
		if n != 1 || c.OperationState != "unknown" {
			return false, database.ErrNotFound
		}
	}
	if v.Schedule {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error='dispatch outcome unknown; reconciliation required' WHERE id=? AND published_at IS NULL`, v.Envelope, v.At, UUIDBytes(c.OutboxID))
	} else {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET published_at=?,locked_by=NULL,locked_until=NULL,last_error='reconciliation attempt limit reached after unknown dispatch outcome' WHERE id=? AND published_at IS NULL`, v.At, UUIDBytes(c.OutboxID))
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
	n, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=?,expires_at=?,updated_at=? WHERE id=? AND state IN ('dispatched','accepted','running')`, v.Envelope, v.ExpiresAt, v.At, UUIDBytes(c.CommandID))
	if err != nil || n != 1 {
		return false, err
	}
	n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=?,updated_at=?,completed_at=NULL WHERE id=? AND state IN ('dispatched','accepted','running','unknown')`, v.ExpiresAt, v.At, UUIDBytes(c.OperationID))
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, database.ErrNotFound
	}
	if v.Schedule {
		n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error='command result missing; reconciliation required' WHERE id=? AND published_at IS NOT NULL AND locked_by IS NULL`, v.Envelope, v.At, UUIDBytes(c.OutboxID))
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, database.ErrNotFound
		}
	} else if _, err := s.Exec(ctx, `UPDATE outbox_events SET last_error='command result missing; reconciliation attempt limit reached' WHERE id=? AND published_at IS NOT NULL AND locked_by IS NULL`, UUIDBytes(c.OutboxID)); err != nil {
		return false, err
	}
	return true, s.recoveryEvent(ctx, c.OperationID, v.At)
}

func (s operationStore) ContinueReconciliation(ctx context.Context, v operationstore.RecoveryUpdate) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE commands SET envelope=?,updated_at=? WHERE id=? AND state='unknown'`, v.Envelope, v.At, UUIDBytes(v.Command.CommandID))
	if err != nil || n != 1 {
		return false, err
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error='reconciliation response missing; continuing reconcile-only delivery' WHERE id=? AND published_at IS NOT NULL AND locked_by IS NULL`, v.Envelope, v.At, UUIDBytes(v.Command.OutboxID))
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, database.ErrNotFound
	}
	return true, nil
}
