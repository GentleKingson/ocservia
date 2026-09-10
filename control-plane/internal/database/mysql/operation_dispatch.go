package mysql

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

// The admission lock serializes claimers. Lock only the outbox rows, then
// re-read eligibility so result ingestion retains its outbox-before-command order.
func (s operationStore) DispatchCandidates(ctx context.Context, limit, available int, at value.Timestamp) ([]operationstore.Dispatch, error) {
	rows, err := s.Query(ctx, `WITH ranked AS (
		SELECT o.id,c.state,o.available_at,
		ROW_NUMBER() OVER(PARTITION BY c.node_id ORDER BY CASE WHEN c.state='unknown' THEN 0 ELSE 1 END,o.available_at,o.id) AS node_rank
		FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN nodes n ON n.id=c.node_id AND n.status='active'
		WHERE o.published_at IS NULL AND o.available_at<=? AND (o.locked_until IS NULL OR o.locked_until<=?)
		AND c.state IN ('queued','unknown') AND c.expires_at>?
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=c.node_id)
		AND (c.resource_type IS NULL OR c.resource_key IS NULL OR NOT EXISTS(
		 SELECT 1 FROM commands p WHERE p.node_id=c.node_id AND BINARY p.resource_type=BINARY c.resource_type
		 AND BINARY p.resource_key=BINARY c.resource_key AND p.id<>c.id AND p.expected_version<c.expected_version
		 AND p.state IN ('queued','dispatched','accepted','running','unknown'))))
		SELECT id,state FROM ranked WHERE node_rank=1 ORDER BY CASE WHEN state='unknown' THEN 0 ELSE 1 END,available_at,id LIMIT ?`, at, at, at, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id    uuid.UUID
		state string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.state); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []operationstore.Dispatch
	for _, c := range candidates {
		if c.state == "queued" {
			if available == 0 {
				continue
			}
			available--
		}
		var d operationstore.Dispatch
		err := s.QueryRow(ctx, `SELECT id,command_id,payload FROM outbox_events
			WHERE id=? AND published_at IS NULL AND available_at<=? AND (locked_until IS NULL OR locked_until<=?)
			FOR UPDATE SKIP LOCKED`, UUIDBytes(c.id), at, at).Scan(&d.OutboxID, &d.CommandID, &d.Envelope)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.traceparent FROM commands c JOIN nodes n ON n.id=c.node_id AND n.status='active'
			WHERE c.id=? AND c.state IN ('queued','unknown') AND c.expires_at>?
			AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=c.node_id)
			AND (c.resource_type IS NULL OR c.resource_key IS NULL OR NOT EXISTS(
			 SELECT 1 FROM commands p WHERE p.node_id=c.node_id AND BINARY p.resource_type=BINARY c.resource_type
			 AND BINARY p.resource_key=BINARY c.resource_key AND p.id<>c.id AND p.expected_version<c.expected_version
			 AND p.state IN ('queued','dispatched','accepted','running','unknown')))`, UUIDBytes(d.CommandID), at).
			Scan(&d.OperationID, &d.NodeID, &d.Traceparent)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}

func (s operationStore) ClaimDispatch(ctx context.Context, d operationstore.Dispatch, worker uuid.UUID, until, at value.Timestamp) (bool, error) {
	_, err := s.Exec(ctx, `INSERT INTO node_command_leases(node_id,command_id,lease_token,worker_id,leased_until,created_at) VALUES(?,?,?,?,?,?)`,
		UUIDBytes(d.NodeID), UUIDBytes(d.CommandID), UUIDBytes(d.LeaseToken), UUIDBytes(worker), until, at)
	if errors.Is(err, database.ErrUnique) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET locked_by=?,locked_until=?,attempts=attempts+1 WHERE id=?`, UUIDBytes(worker), until, UUIDBytes(d.OutboxID)); err != nil {
		return false, err
	}
	var attempt int
	if err := s.QueryRow(ctx, `SELECT attempts FROM outbox_events WHERE id=?`, UUIDBytes(d.OutboxID)).Scan(&attempt); err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `INSERT INTO command_attempts(id,command_id,outbox_event_id,worker_id,attempt_number,state,started_at) VALUES(?,?,?,?,?,'sending',?)`,
		UUIDBytes(d.AttemptID), UUIDBytes(d.CommandID), UUIDBytes(d.OutboxID), UUIDBytes(worker), attempt, at)
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
	return s.QueryRow(ctx, `SELECT /* mark_sent_outbox_lock */ id FROM outbox_events WHERE id=? AND command_id=? FOR UPDATE`, UUIDBytes(d.OutboxID), UUIDBytes(d.CommandID)).Scan(&id)
}

func (s operationStore) DispatchStatus(ctx context.Context, d operationstore.Dispatch) (v operationstore.DispatchStatus, err error) {
	err = s.QueryRow(ctx, `SELECT l.leased_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)),
		a.state='sending' AND a.finished_at IS NULL AND a.worker_id=l.worker_id AND a.attempt_number=o.attempts,
		COALESCE(o.locked_by=l.worker_id AND o.locked_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)),false),
		EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.created_at>=a.started_at),c.state
		FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id
		JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
		JOIN command_attempts a ON a.id=? AND a.command_id=c.id AND a.outbox_event_id=o.id
		WHERE o.id=? AND c.id=? AND p.id=? AND c.node_id=? AND l.lease_token=?`,
		UUIDBytes(d.AttemptID), UUIDBytes(d.OutboxID), UUIDBytes(d.CommandID), UUIDBytes(d.OperationID), UUIDBytes(d.NodeID), UUIDBytes(d.LeaseToken)).
		Scan(&v.LeaseValid, &v.AttemptValid, &v.OwnsOutboxLock, &v.ResultAfterAttempt, &v.CommandState)
	return
}

func (s operationStore) CompletedDispatch(ctx context.Context, d operationstore.Dispatch) (v operationstore.DispatchStatus, err error) {
	err = s.QueryRow(ctx, `SELECT c.state,
		EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.created_at>=a.started_at)
		FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id
		JOIN command_attempts a ON a.id=? AND a.command_id=c.id AND a.outbox_event_id=o.id
		WHERE o.id=? AND c.id=? AND p.id=? AND c.node_id=? AND a.state='sent' AND a.finished_at IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=c.node_id AND l.command_id=c.id AND l.lease_token=?)`,
		UUIDBytes(d.AttemptID), UUIDBytes(d.OutboxID), UUIDBytes(d.CommandID), UUIDBytes(d.OperationID), UUIDBytes(d.NodeID), UUIDBytes(d.LeaseToken)).Scan(&v.CommandState, &v.ResultAfterAttempt)
	return
}

func (s operationStore) SaveTerminalEnvelope(ctx context.Context, id uuid.UUID, envelope []byte) error {
	_, err := s.Exec(ctx, `UPDATE commands SET envelope=? WHERE id=?`, envelope, UUIDBytes(id))
	return err
}

func (s operationStore) PublishDispatch(ctx context.Context, d operationstore.Dispatch, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=?,locked_by=NULL,locked_until=NULL,last_error=NULL
		WHERE id=? AND locked_by=(SELECT worker_id FROM node_command_leases WHERE lease_token=?)`, at, UUIDBytes(d.OutboxID), UUIDBytes(d.LeaseToken))
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) RecordDispatched(ctx context.Context, d operationstore.Dispatch, envelope []byte, unknown bool, at value.Timestamp) error {
	if unknown {
		_, err := s.Exec(ctx, `UPDATE commands SET envelope=?,updated_at=? WHERE id=? AND state='unknown'`, envelope, at, UUIDBytes(d.CommandID))
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE commands SET state='dispatched',envelope=?,updated_at=? WHERE id=? AND state IN ('queued','unknown')`, envelope, at, UUIDBytes(d.CommandID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET state='dispatched',version=version+1,updated_at=? WHERE id=? AND state='queued'`, at, UUIDBytes(d.OperationID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='dispatched',updated_at=? WHERE operation_id=? AND state IN ('queued','unknown')`, at, UUIDBytes(d.OperationID)); err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) VALUES(?,?,'dispatched',?)`, UUIDBytes(id), UUIDBytes(d.OperationID), at)
	return err
}

func (s operationStore) CloseDispatch(ctx context.Context, d operationstore.Dispatch, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=? WHERE id=? AND command_id=? AND outbox_event_id=? AND state='sending'`, at, UUIDBytes(d.AttemptID), UUIDBytes(d.CommandID), UUIDBytes(d.OutboxID))
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=? AND command_id=? AND lease_token=?`, UUIDBytes(d.NodeID), UUIDBytes(d.CommandID), UUIDBytes(d.LeaseToken))
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) FailDispatch(ctx context.Context, d operationstore.Dispatch, message string, at value.Timestamp) error {
	var valid bool
	if err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_command_leases WHERE node_id=? AND command_id=? AND lease_token=? AND leased_until>?)`, UUIDBytes(d.NodeID), UUIDBytes(d.CommandID), UUIDBytes(d.LeaseToken), at).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return database.ErrNotFound
	}
	retry, err := at.Add(time.Second)
	if err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET locked_by=NULL,locked_until=NULL,available_at=?,last_error=? WHERE id=? AND locked_by IS NOT NULL`, retry, message, UUIDBytes(d.OutboxID)); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE command_attempts SET state='failed',finished_at=?,error_code='transport_unavailable' WHERE id=? AND state='sending'`, at, UUIDBytes(d.AttemptID)); err != nil {
		return err
	}
	_, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE lease_token=?`, UUIDBytes(d.LeaseToken))
	return err
}

func (s operationStore) ExtendDispatch(ctx context.Context, d operationstore.Dispatch, worker uuid.UUID, duration time.Duration) error {
	if err := s.LockDispatchOutbox(ctx, d); err != nil {
		return err
	}
	var owner uuid.UUID
	var leaseUntil, lockUntil value.Timestamp
	err := s.QueryRow(ctx, `SELECT l.worker_id,l.leased_until,o.locked_until FROM outbox_events o
		JOIN node_command_leases l ON l.command_id=? AND l.node_id=?
		JOIN command_attempts a ON a.id=? AND a.command_id=? AND a.outbox_event_id=?
		WHERE o.id=? AND o.command_id=? AND o.published_at IS NULL AND l.lease_token=?
		AND o.locked_by=l.worker_id AND a.worker_id=l.worker_id AND a.state='sending' AND a.finished_at IS NULL FOR UPDATE`,
		UUIDBytes(d.CommandID), UUIDBytes(d.NodeID), UUIDBytes(d.AttemptID), UUIDBytes(d.CommandID), UUIDBytes(d.OutboxID), UUIDBytes(d.OutboxID), UUIDBytes(d.CommandID), UUIDBytes(d.LeaseToken)).Scan(&owner, &leaseUntil, &lockUntil)
	if err != nil {
		return err
	}
	if owner != worker {
		return errors.New("pre-send claim belongs to another worker")
	}
	var at value.Timestamp
	if err := s.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))`).Scan(&at); err != nil {
		return err
	}
	if !leaseUntil.Valid || !lockUntil.Valid || leaseUntil.Micros <= at.Micros || lockUntil.Micros <= at.Micros {
		return database.ErrNotFound
	}
	until, err := at.Add(duration)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE node_command_leases SET leased_until=? WHERE lease_token=? AND worker_id=?`, until, UUIDBytes(d.LeaseToken), UUIDBytes(worker))
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET locked_until=? WHERE id=? AND command_id=? AND locked_by=?`, until, UUIDBytes(d.OutboxID), UUIDBytes(d.CommandID), UUIDBytes(worker))
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s operationStore) QueueMetrics(ctx context.Context) (v operationstore.QueueMetrics, err error) {
	at, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	var oldest value.Timestamp
	err = s.QueryRow(ctx, `SELECT COUNT(CASE WHEN published_at IS NULL THEN 1 END),MIN(CASE WHEN published_at IS NULL THEN created_at END),
		(SELECT COUNT(*) FROM commands WHERE state='queued'),(SELECT COUNT(*) FROM commands WHERE state='unknown'),
		(SELECT COUNT(*) FROM config_apply_operations WHERE state='rolled_back'),(SELECT COUNT(*) FROM config_apply_operations WHERE state='failed_critical') FROM outbox_events`).
		Scan(&v.Unpublished, &oldest, &v.Queued, &v.Unknown, &v.ConfigRollbacks, &v.ConfigFailedCritical)
	if err != nil {
		return v, err
	}
	if oldest.Valid {
		if oldest.Micros == value.NegativeInfinity || oldest.Micros == value.PositiveInfinity {
			return v, errors.New("operations: infinite queue age")
		}
		v.OldestAge = (float64(at.Micros) - float64(oldest.Micros)) / 1e6
	}
	return v, nil
}
