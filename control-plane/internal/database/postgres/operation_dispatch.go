package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) DispatchCandidates(ctx context.Context, limit, available int, at value.Timestamp) ([]operationstore.Dispatch, error) {
	rows, err := s.Query(ctx, `WITH ranked AS (
		SELECT outbox.id AS outbox_id,command.id AS command_id,command.state,outbox.available_at,
		row_number() OVER(PARTITION BY command.node_id ORDER BY CASE WHEN command.state='unknown' THEN 0 ELSE 1 END,outbox.available_at,outbox.id) AS node_rank
		FROM outbox_events AS outbox JOIN commands AS command ON command.id=outbox.command_id
		JOIN nodes AS node ON node.id=command.node_id AND node.status='active'
		WHERE outbox.published_at IS NULL AND outbox.available_at<=$3
		AND (outbox.locked_until IS NULL OR outbox.locked_until<=$3)
		AND command.state IN ('queued','unknown') AND command.expires_at>$3
		AND (command.state='queued' OR outbox.attempts<LEAST($4,COALESCE(outbox.receipt_recovery_attempt_limit,$4)))
		AND NOT EXISTS(SELECT 1 FROM node_command_leases lease WHERE lease.node_id=command.node_id)
		AND (command.resource_type IS NULL OR command.resource_key IS NULL OR NOT EXISTS (
		 SELECT 1 FROM commands AS prior WHERE prior.node_id=command.node_id AND prior.resource_type=command.resource_type
		 AND prior.resource_key=command.resource_key AND prior.id<>command.id AND prior.expected_version<command.expected_version
		 AND prior.state IN ('queued','dispatched','accepted','running','unknown')))
	), unknown_candidates AS (
		SELECT * FROM ranked WHERE node_rank=1 AND state='unknown' ORDER BY available_at,outbox_id LIMIT LEAST($1::bigint,$2::bigint)
	), queued_candidates AS (
		SELECT * FROM ranked WHERE node_rank=1 AND state='queued' ORDER BY available_at,outbox_id
		LIMIT GREATEST(0,LEAST($1::bigint,$2::bigint)-(SELECT count(*) FROM unknown_candidates))
	), selected AS (SELECT * FROM unknown_candidates UNION ALL SELECT * FROM queued_candidates)
	SELECT outbox.id,command.id,command.operation_id,command.node_id,outbox.payload,command.traceparent
	FROM selected JOIN outbox_events AS outbox ON outbox.id=selected.outbox_id JOIN commands AS command ON command.id=selected.command_id
	ORDER BY CASE WHEN selected.state='unknown' THEN 0 ELSE 1 END,selected.available_at,selected.outbox_id
	FOR UPDATE OF outbox SKIP LOCKED`, limit, available, at, operationstore.ReconciliationAttemptLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []operationstore.Dispatch
	for rows.Next() {
		var d operationstore.Dispatch
		if err := rows.Scan(&d.OutboxID, &d.CommandID, &d.OperationID, &d.NodeID, &d.Envelope, &d.Traceparent); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s operationStore) ClaimDispatch(ctx context.Context, d operationstore.Dispatch, worker uuid.UUID, until, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(node_id) DO NOTHING`, d.NodeID, d.CommandID, d.LeaseToken, worker, until, at)
	if err != nil || n == 0 {
		return false, err
	}
	var attempt int
	if err := s.QueryRow(ctx, `UPDATE outbox_events SET locked_by=$2,locked_until=$3,attempts=attempts+1 WHERE id=$1 RETURNING attempts`, d.OutboxID, worker, until).Scan(&attempt); err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at)
		VALUES($1,$2,$3,$4,$5,'sending',$6)`, d.AttemptID, d.CommandID, d.OutboxID, worker, attempt, at)
	return err == nil, err
}

func (s operationStore) GuardDispatchAuthority(ctx context.Context, a operationstore.DispatchAuthority) error {
	return (connectionOwnerStore{s.Tx}).Assert(ctx, connectionowner.StoreTerm{
		Identity: connectionowner.Identity{InstanceID: a.OwnerID, Incarnation: a.Incarnation},
		NodeID:   a.NodeID, ConnectionID: a.ConnectionID, Epoch: a.Epoch,
	})
}

func (s operationStore) LockDispatchOutbox(ctx context.Context, d operationstore.Dispatch) error {
	var id uuid.UUID
	return s.QueryRow(ctx, `SELECT /* mark_sent_outbox_lock */ id FROM outbox_events WHERE id=$1 AND command_id=$2 FOR UPDATE`, d.OutboxID, d.CommandID).Scan(&id)
}

func (s operationStore) DispatchStatus(ctx context.Context, d operationstore.Dispatch) (v operationstore.DispatchStatus, err error) {
	err = s.QueryRow(ctx, `SELECT lease.leased_until>clock_timestamp(),
		attempt.state='sending' AND attempt.finished_at IS NULL AND attempt.worker_id=lease.worker_id AND attempt.attempt_number=outbox.attempts,
		COALESCE(outbox.locked_by=lease.worker_id AND outbox.locked_until>clock_timestamp(),false),
		EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.created_at>=attempt.started_at),command.state
		FROM outbox_events AS outbox JOIN commands AS command ON command.id=outbox.command_id
		JOIN operations AS operation ON operation.id=command.operation_id
		JOIN node_command_leases AS lease ON lease.command_id=command.id AND lease.node_id=command.node_id
		JOIN command_attempts AS attempt ON attempt.id=$5 AND attempt.command_id=command.id AND attempt.outbox_event_id=outbox.id
		WHERE outbox.id=$1 AND command.id=$2 AND operation.id=$3 AND command.node_id=$4 AND lease.lease_token=$6`,
		d.OutboxID, d.CommandID, d.OperationID, d.NodeID, d.AttemptID, d.LeaseToken).
		Scan(&v.LeaseValid, &v.AttemptValid, &v.OwnsOutboxLock, &v.ResultAfterAttempt, &v.CommandState)
	return
}

func (s operationStore) CompletedDispatch(ctx context.Context, d operationstore.Dispatch) (v operationstore.DispatchStatus, err error) {
	err = s.QueryRow(ctx, `SELECT command.state,command.envelope,
		EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.created_at>=attempt.started_at)
		FROM outbox_events AS outbox JOIN commands AS command ON command.id=outbox.command_id
		JOIN operations AS operation ON operation.id=command.operation_id
		JOIN command_attempts AS attempt ON attempt.id=$5 AND attempt.command_id=command.id AND attempt.outbox_event_id=outbox.id
		WHERE outbox.id=$1 AND command.id=$2 AND operation.id=$3 AND command.node_id=$4
		AND attempt.state='sent' AND attempt.finished_at IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.node_id=command.node_id AND lease.command_id=command.id AND lease.lease_token=$6)`,
		d.OutboxID, d.CommandID, d.OperationID, d.NodeID, d.AttemptID, d.LeaseToken).Scan(&v.CommandState, &v.Envelope, &v.ResultAfterAttempt)
	return
}

func (s operationStore) SaveTerminalEnvelope(ctx context.Context, id uuid.UUID, envelope []byte) error {
	_, err := s.Exec(ctx, `UPDATE commands SET envelope=$2 WHERE id=$1 AND details_compacted_at IS NULL`, id, envelope)
	return err
}

func (s operationStore) PublishDispatch(ctx context.Context, d operationstore.Dispatch, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=$3,locked_by=NULL,locked_until=NULL,last_error=NULL
		WHERE id=$1 AND locked_by=(SELECT worker_id FROM node_command_leases WHERE lease_token=$2)`, d.OutboxID, d.LeaseToken, at)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) RecordDispatched(ctx context.Context, d operationstore.Dispatch, envelope []byte, unknown bool, at value.Timestamp) error {
	if unknown {
		_, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,updated_at=$3 WHERE id=$1 AND state='unknown'`, d.CommandID, envelope, at)
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE commands SET state='dispatched',envelope=$2,updated_at=$3 WHERE id=$1 AND state IN ('queued','unknown')`, d.CommandID, envelope, at); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET state='dispatched',version=version+1,updated_at=$2 WHERE id=$1 AND state='queued'`, d.OperationID, at); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched',updated_at=$2 WHERE operation_id=$1 AND state IN ('queued','unknown')`, d.OperationID, at); err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES($1,$2,'dispatched',$3)`, id, d.OperationID, at)
	return err
}

func (s operationStore) CloseDispatch(ctx context.Context, d operationstore.Dispatch, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=$4 WHERE id=$1 AND command_id=$2 AND outbox_event_id=$3 AND state='sending'`, d.AttemptID, d.CommandID, d.OutboxID, at)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=$1 AND command_id=$2 AND lease_token=$3`, d.NodeID, d.CommandID, d.LeaseToken)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) FailDispatch(ctx context.Context, d operationstore.Dispatch, message string, at value.Timestamp) error {
	var valid bool
	if err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_command_leases WHERE node_id=$1 AND command_id=$2 AND lease_token=$3 AND leased_until>$4)`, d.NodeID, d.CommandID, d.LeaseToken, at).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return database.ErrNotFound
	}
	retry, err := at.Add(time.Second)
	if err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events o SET locked_by=NULL,locked_until=NULL,available_at=$3,last_error=$2,
		published_at=CASE WHEN c.state='unknown' AND o.attempts>=LEAST($5,COALESCE(o.receipt_recovery_attempt_limit,$5))
			THEN $4 ELSE o.published_at END
		FROM commands c WHERE o.id=$1 AND c.id=o.command_id AND o.locked_by IS NOT NULL`,
		d.OutboxID, message, retry, at, operationstore.ReconciliationAttemptLimit); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE command_attempts SET state='failed',finished_at=$2,error_code='transport_unavailable' WHERE id=$1 AND state='sending'`, d.AttemptID, at); err != nil {
		return err
	}
	_, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE lease_token=$1`, d.LeaseToken)
	return err
}

func (s operationStore) ExtendDispatch(ctx context.Context, d operationstore.Dispatch, worker uuid.UUID, duration time.Duration) error {
	var owner uuid.UUID
	err := s.QueryRow(ctx, `SELECT lease.worker_id FROM outbox_events AS outbox
		JOIN node_command_leases AS lease ON lease.command_id=$2 AND lease.node_id=$3
		JOIN command_attempts AS attempt ON attempt.id=$4 AND attempt.command_id=$2 AND attempt.outbox_event_id=$1
		WHERE outbox.id=$1 AND outbox.command_id=$2 AND outbox.published_at IS NULL
		AND lease.lease_token=$5 AND lease.leased_until>clock_timestamp()
		AND outbox.locked_by=lease.worker_id AND outbox.locked_until>clock_timestamp()
		AND attempt.worker_id=lease.worker_id AND attempt.state='sending' AND attempt.finished_at IS NULL
		FOR UPDATE OF outbox,lease,attempt`, d.OutboxID, d.CommandID, d.NodeID, d.AttemptID, d.LeaseToken).Scan(&owner)
	if err != nil {
		return err
	}
	if owner != worker {
		return errors.New("pre-send claim belongs to another worker")
	}
	// Read wall time after FOR UPDATE acquires row locks so lease extension
	// never uses a pre-wait timestamp.
	var at value.Timestamp
	if err := s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return err
	}
	until, err := at.Add(duration)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE node_command_leases SET leased_until=$2 WHERE lease_token=$1 AND worker_id=$3`, d.LeaseToken, until, worker)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET locked_until=$2 WHERE id=$1 AND command_id=$3 AND locked_by=$4`, d.OutboxID, until, d.CommandID, worker)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) RecoveryClock(ctx context.Context) (at value.Timestamp, err error) {
	err = s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return
}

func (s operationStore) ExpiredApplyCommands(ctx context.Context) ([]operationstore.RecoveryCommand, error) {
	ids, err := database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT c.id FROM outbox_events o JOIN commands c ON c.id=o.command_id
		JOIN config_apply_operations a ON a.operation_id=c.operation_id
		WHERE c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=now()
		ORDER BY c.id FOR UPDATE OF o SKIP LOCKED`))
	if err != nil {
		return nil, err
	}
	var result []operationstore.RecoveryCommand
	for _, id := range ids {
		v := operationstore.RecoveryCommand{CommandID: id}
		err := s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,c.expires_at,o.attempts,LEAST($2,COALESCE(o.receipt_recovery_attempt_limit,$2))
			FROM commands c JOIN config_apply_operations a ON a.operation_id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
			WHERE c.id=$1 AND c.state IN ('dispatched','accepted','running') AND a.state IN ('dispatched','accepted','running') AND c.expires_at<=now() FOR UPDATE OF c`,
			id, operationstore.ReconciliationAttemptLimit).Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.ExpiresAt, &v.Attempts, &v.AttemptLimit)
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
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT c.id`+reapingSendingFrom+` ORDER BY l.leased_until,c.id LIMIT $1 FOR UPDATE OF o SKIP LOCKED`, limit))
}

func (s operationStore) LockExpiredSendingCommand(ctx context.Context, id uuid.UUID) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,a.id,c.state,p.state,o.attempts,c.expires_at,LEAST($2,COALESCE(o.receipt_recovery_attempt_limit,$2))`+reapingSendingFrom+` AND c.id=$1 FOR UPDATE OF c,p`,
		id, operationstore.ReconciliationAttemptLimit).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.AttemptID, &v.CommandState, &v.OperationState, &v.Attempts, &v.ExpiresAt, &v.AttemptLimit)
	return
}

const reapingSentFrom = ` FROM commands c JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
	JOIN LATERAL(SELECT state,finished_at,attempt_number FROM command_attempts WHERE command_id=c.id ORDER BY attempt_number DESC LIMIT 1) a ON true
	WHERE c.state IN ('dispatched','accepted','running') AND p.state IN ('dispatched','accepted','running','unknown')
	AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
	AND o.published_at IS NOT NULL AND o.locked_by IS NULL AND a.state='sent' AND a.attempt_number=o.attempts
	AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)`

func (s operationStore) StaleSentCommands(ctx context.Context, timeout time.Duration, limit int) ([]uuid.UUID, error) {
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT c.id`+reapingSentFrom+` AND a.finished_at<=clock_timestamp()-$1::interval ORDER BY a.finished_at,c.id LIMIT $2 FOR UPDATE OF o SKIP LOCKED`, timeout.String(), limit))
}

func (s operationStore) LockStaleSentCommand(ctx context.Context, id uuid.UUID, timeout time.Duration) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,o.attempts,c.expires_at,LEAST($3,COALESCE(o.receipt_recovery_attempt_limit,$3))`+reapingSentFrom+`
		AND c.id=$1 AND a.finished_at<=clock_timestamp()-$2::interval FOR UPDATE OF c,p`, id, timeout.String(), operationstore.ReconciliationAttemptLimit).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.Attempts, &v.ExpiresAt, &v.AttemptLimit)
	return
}

func (s operationStore) StaleReconciliations(ctx context.Context, maxAttempts int, timeout time.Duration, limit int) ([]operationstore.RecoveryCommand, error) {
	rows, err := s.Query(ctx, `SELECT c.id,c.operation_id,c.node_id,c.envelope,o.id FROM commands c
		JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
		JOIN LATERAL(SELECT state,started_at,finished_at FROM command_attempts WHERE command_id=c.id ORDER BY attempt_number DESC LIMIT 1) a ON true
		WHERE c.state='unknown' AND p.state='unknown' AND o.published_at IS NOT NULL AND o.locked_by IS NULL
		AND o.attempts<LEAST($1,COALESCE(o.receipt_recovery_attempt_limit,$1)) AND a.state='sent' AND a.finished_at<=clock_timestamp()-$2::interval AND c.expires_at>clock_timestamp()
		AND NOT EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.created_at>=a.started_at)
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
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=CASE WHEN $4 THEN NULL ELSE $3::timestamptz END,locked_by=NULL,locked_until=NULL,available_at=$3,
		last_error=CASE WHEN $4 THEN 'apply outcome missing; reconciliation required' ELSE 'apply reconciliation limit or deadline reached' END
		WHERE command_id=$1`, v.Command.CommandID, v.Envelope, v.At, v.Schedule); err != nil {
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

func (s operationStore) ReconnectAuthority(ctx context.Context, node, connection uuid.UUID, epoch int64) (v operationstore.RecoveryAuthority, err error) {
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation FROM connection_owner_fencing
		WHERE node_id=$1 AND connection_id=$2 AND owner_epoch=$3 AND lease_until>clock_timestamp()
		FOR SHARE OF connection_owner_fencing`, node[:], connection[:], epoch).Scan(&v.OwnerID, &v.Incarnation)
	return
}

func (s operationStore) AmbiguousDispatches(ctx context.Context, node uuid.UUID, limit int) ([]uuid.UUID, error) {
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT command.id FROM commands AS command JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.node_id=$1 AND command.state IN ('dispatched','accepted','running','unknown')
		AND operation.state IN ('dispatched','accepted','running','unknown')
		AND NOT (command.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.state='succeeded' AND result.receipt_verification_status='verified'))
		AND outbox.published_at IS NOT NULL AND outbox.locked_by IS NULL
		AND outbox.attempts<LEAST($3,COALESCE(outbox.receipt_recovery_attempt_limit,$3))
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		ORDER BY command.created_at,command.id LIMIT $2 FOR UPDATE OF outbox SKIP LOCKED`, node, limit, operationstore.ReconciliationAttemptLimit))
}

func (s operationStore) LockAmbiguousDispatch(ctx context.Context, command, node uuid.UUID) (v operationstore.AmbiguousDispatch, err error) {
	err = s.QueryRow(ctx, `SELECT command.operation_id,command.envelope,command.expires_at FROM commands AS command
		JOIN operations AS operation ON operation.id=command.operation_id JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1 AND command.node_id=$2 AND command.state IN ('dispatched','accepted','running','unknown')
		AND operation.state IN ('dispatched','accepted','running','unknown')
		AND NOT (command.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.state='succeeded' AND result.receipt_verification_status='verified'))
		AND outbox.published_at IS NOT NULL AND outbox.locked_by IS NULL
		AND outbox.attempts<LEAST($3,COALESCE(outbox.receipt_recovery_attempt_limit,$3))
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		FOR UPDATE OF command,operation`, command, node, operationstore.ReconciliationAttemptLimit).Scan(&v.OperationID, &v.Envelope, &v.ExpiresAt)
	return
}

func (s operationStore) UpgradeSchedulingAcked(ctx context.Context, operation uuid.UUID) (acked bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands AS command JOIN agent_command_results AS result ON result.command_id=command.id
		WHERE command.operation_id=$1 AND command.payload_type='agent_upgrade' AND result.state='succeeded' AND result.receipt_verification_status='verified')`, operation).Scan(&acked)
	return
}

func (s operationStore) MarkRecoveryProjection(ctx context.Context, v operationstore.RecoveryProjection) error {
	if v.ConfigApply {
		if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=$2 WHERE operation_id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, v.OperationID, v.At); err != nil {
			return err
		}
	}
	if v.CertificateID != uuid.Nil {
		if _, err := s.Exec(ctx, `UPDATE certificates SET state='unknown',version=version+1,updated_at=$2 WHERE id=$1 AND state NOT IN ('issued','expired','revoked','failed')`, v.CertificateID, v.At); err != nil {
			return err
		}
	}
	if v.Artifact {
		if _, err := s.Exec(ctx, `UPDATE artifact_operations SET state='pending',updated_at=$2 WHERE operation_id=$1 AND state IN ('pending','leased')`, v.OperationID, v.At); err != nil {
			return err
		}
	}
	return nil
}

func (s operationStore) ScheduleReconnectRecovery(ctx context.Context, command, operation uuid.UUID, payload []byte, expires, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=$2,expires_at=$3,updated_at=$4 WHERE id=$1 AND state IN ('dispatched','accepted','running','unknown')`, command, payload, expires, at)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=$2,updated_at=$3,completed_at=NULL WHERE id=$1 AND state IN ('dispatched','accepted','running','unknown')`, operation, expires, at)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error='owner connection changed; reconciliation required'
		WHERE command_id=$1 AND published_at IS NOT NULL AND locked_by IS NULL`, command, payload, at)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'unknown',$3)`, id, operation, at)
	return err
}
