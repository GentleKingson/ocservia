package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
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
		AND NOT EXISTS(SELECT 1 FROM node_command_leases lease WHERE lease.node_id=command.node_id)
		AND (command.resource_type IS NULL OR command.resource_key IS NULL OR NOT EXISTS (
		 SELECT 1 FROM commands AS prior WHERE prior.node_id=command.node_id AND prior.resource_type=command.resource_type
		 AND prior.resource_key=command.resource_key AND prior.id<>command.id AND prior.expected_version<command.expected_version
		 AND prior.state IN ('queued','dispatched','accepted','running','unknown')))
	), unknown_candidates AS (
		SELECT * FROM ranked WHERE node_rank=1 AND state='unknown' ORDER BY available_at,outbox_id LIMIT $1
	), queued_candidates AS (
		SELECT * FROM ranked WHERE node_rank=1 AND state='queued' ORDER BY available_at,outbox_id
		LIMIT LEAST($2,GREATEST(0,$1-(SELECT count(*) FROM unknown_candidates)))
	), selected AS (SELECT * FROM unknown_candidates UNION ALL SELECT * FROM queued_candidates)
	SELECT outbox.id,command.id,command.operation_id,command.node_id,outbox.payload,command.traceparent
	FROM selected JOIN outbox_events AS outbox ON outbox.id=selected.outbox_id JOIN commands AS command ON command.id=selected.command_id
	ORDER BY CASE WHEN selected.state='unknown' THEN 0 ELSE 1 END,selected.available_at,selected.outbox_id
	FOR UPDATE OF outbox SKIP LOCKED`, limit, available, at)
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
	return (connectionOwnerStore{s.Tx}).Assert(ctx, ownerstore.Term{
		Identity: ownerstore.Identity{InstanceID: a.OwnerID, Incarnation: a.Incarnation},
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
	_, err := s.Exec(ctx, `UPDATE commands SET envelope=$2 WHERE id=$1`, id, envelope)
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
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET locked_by=NULL,locked_until=NULL,available_at=$3,last_error=$2 WHERE id=$1 AND locked_by IS NOT NULL`, d.OutboxID, message, retry); err != nil {
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

func (s operationStore) QueueMetrics(ctx context.Context) (v operationstore.QueueMetrics, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FILTER(WHERE published_at IS NULL),COALESCE(extract(epoch FROM now()-min(created_at) FILTER(WHERE published_at IS NULL)),0),
		(SELECT count(*) FROM commands WHERE state='queued'),(SELECT count(*) FROM commands WHERE state='unknown'),
		(SELECT count(*) FROM config_apply_operations WHERE state='rolled_back'),(SELECT count(*) FROM config_apply_operations WHERE state='failed_critical') FROM outbox_events`).
		Scan(&v.Unpublished, &v.OldestAge, &v.Queued, &v.Unknown, &v.ConfigRollbacks, &v.ConfigFailedCritical)
	return
}
