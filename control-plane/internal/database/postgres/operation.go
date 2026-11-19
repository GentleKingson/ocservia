package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	resultstore "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

func (s operationStore) LockNode(ctx context.Context, id uuid.UUID) (v operationstore.NodeState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,version,authorization_revision,status FROM nodes WHERE id=$1 FOR UPDATE`, id).Scan(&v.WorkspaceID, &v.Version, &v.AuthorizationRevision, &v.Status)
	return
}

func (s operationStore) HasCapability(ctx context.Context, node uuid.UUID, capability string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability=$2 AND approved=true)`, node, capability).Scan(&v)
	return
}

func (s operationStore) AttestationReady(ctx context.Context, node uuid.UUID) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability='privd_result_attestation_v1' AND approved=true)
		AND EXISTS(SELECT 1 FROM node_privd_attestation_keys WHERE node_id=$1 AND state='active' AND (valid_until IS NULL OR valid_until>now()))`, node).Scan(&v)
	return
}

func (s operationStore) HasSession(ctx context.Context, node uuid.UUID, session, boot string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_sessions s JOIN node_observed_snapshots o ON o.node_id=s.node_id WHERE s.node_id=$1 AND s.session_id=$2 AND o.boot_id=$3)`, node, session, boot).Scan(&v)
	return
}

func (s operationStore) HasIPBan(ctx context.Context, node uuid.UUID, ip string) (v bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_ip_bans WHERE node_id=$1 AND ip=$2::inet)`, node, ip).Scan(&v)
	return
}

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
		SELECT * FROM ranked WHERE node_rank=1 AND state='unknown' ORDER BY available_at,outbox_id LIMIT LEAST($1::bigint,$2::bigint)
	), queued_candidates AS (
		SELECT * FROM ranked WHERE node_rank=1 AND state='queued' ORDER BY available_at,outbox_id
		LIMIT GREATEST(0,LEAST($1::bigint,$2::bigint)-(SELECT count(*) FROM unknown_candidates))
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

func (s operationStore) QueueMetrics(ctx context.Context, workspaceID uuid.UUID, all bool) (v operationstore.QueueMetrics, err error) {
	err = s.QueryRow(ctx, `SELECT count(*) FILTER(WHERE o.published_at IS NULL),COALESCE(extract(epoch FROM now()-min(o.created_at) FILTER(WHERE o.published_at IS NULL)),0),
		(SELECT count(*) FROM commands WHERE state='queued' AND ($1::boolean OR workspace_id=$2)),(SELECT count(*) FROM commands WHERE state='unknown' AND ($1::boolean OR workspace_id=$2)),
		(SELECT count(*) FROM config_apply_operations WHERE state='rolled_back' AND ($1::boolean OR workspace_id=$2)),(SELECT count(*) FROM config_apply_operations WHERE state='failed_critical' AND ($1::boolean OR workspace_id=$2))
		FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE $1::boolean OR c.workspace_id=$2`, all, workspaceID).
		Scan(&v.Unpublished, &v.OldestAge, &v.Queued, &v.Unknown, &v.ConfigRollbacks, &v.ConfigFailedCritical)
	return
}

func (s operationStore) ExpireQueued(ctx context.Context) error {
	// A leased command has an uncertain dispatch outcome. Only reconciliation
	// may resolve it, even when its execution deadline has already elapsed.
	ids, err := database.CollectColumn[uuid.UUID](s.Query(ctx, `WITH expired AS (
		UPDATE commands SET state='expired',updated_at=now()
		WHERE state='queued' AND expires_at<=now()
		AND NOT EXISTS(SELECT 1 FROM node_command_leases lease WHERE lease.command_id=commands.id)
		RETURNING id,operation_id
	), stopped AS (
		UPDATE outbox_events SET published_at=now(),locked_by=NULL,locked_until=NULL,last_error='command expired before dispatch'
		FROM expired WHERE outbox_events.command_id=expired.id RETURNING expired.operation_id
	) UPDATE operations SET state='expired',version=version+1,updated_at=now(),completed_at=now()
	FROM stopped WHERE operations.id=stopped.operation_id AND operations.state='queued' RETURNING operations.id`))
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

type operationStore struct{ database.Tx }

func (t *transaction) OperationStore() operationstore.Store { return operationStore{t} }

const operationColumns = `o.id::text,o.state,o.node_id::text,o.command_id::text,o.version,o.created_at,o.updated_at,o.expires_at,COALESCE(x.state,''),COALESCE(x.failure_code,''),COALESCE(u.state,''),COALESCE(u.target_version,'')`
const operationJoins = ` FROM operations o LEFT JOIN config_apply_operations x ON x.operation_id=o.id LEFT JOIN agent_upgrade_operations u ON u.operation_id=o.id`

func scanOperation(row database.Row, same *bool) (v operationstore.Operation, err error) {
	var expires value.Timestamp
	args := []any{&v.ID, &v.State, &v.NodeID, &v.CommandID, &v.Version, &v.CreatedAt, &v.UpdatedAt, &expires, &v.ConfigApplyState, &v.ConfigApplyFailureCode, &v.AgentUpgradeState, &v.AgentUpgradeTarget}
	if same != nil {
		args = append(args, same)
	}
	err = row.Scan(args...)
	if err != nil {
		return operationstore.Operation{}, err
	}
	if expires.Valid {
		v.ExpiresAt = &expires
	}
	return
}

func (s operationStore) Get(ctx context.Context, id uuid.UUID) (operationstore.Operation, error) {
	return scanOperation(s.QueryRow(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.id=$1`, id), nil)
}

func (s operationStore) ListInWorkspace(ctx context.Context, workspace, after uuid.UUID, limit int) ([]operationstore.Operation, error) {
	var workspaceID, cursor any
	if workspace != uuid.Nil {
		workspaceID = workspace
	}
	if after != uuid.Nil {
		cursor = after
	}
	rows, err := s.Query(ctx, `SELECT `+operationColumns+operationJoins+` WHERE ($1::uuid IS NULL OR o.id<$1) AND ($2::uuid IS NULL OR o.workspace_id=$2) ORDER BY o.id DESC LIMIT $3`, cursor, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]operationstore.Operation, 0, limit)
	for rows.Next() {
		v, err := scanOperation(rows, nil)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) SummaryInWorkspace(ctx context.Context, workspace uuid.UUID) (v operationstore.Summary, err error) {
	var workspaceID any
	if workspace != uuid.Nil {
		workspaceID = workspace
	}
	err = s.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state NOT IN ('succeeded','failed','expired','rolled_back','drifted','superseded','unknown')),count(*) FILTER (WHERE state='unknown') FROM operations WHERE ($1::uuid IS NULL OR workspace_id=$1)`, workspaceID).Scan(&v.Active, &v.Unknown)
	return
}

func (s operationStore) FindIdempotent(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v operationstore.Operation, same bool, err error) {
	v, err = scanOperation(s.QueryRow(ctx, `SELECT `+operationColumns+`,o.request_hash=$3`+operationJoins+` WHERE o.workspace_id=$1 AND o.idempotency_key=$2`, workspace, key, hash), &same)
	if errors.Is(err, database.ErrNotFound) {
		return operationstore.Operation{}, false, nil
	}
	return
}

func (s operationStore) Events(ctx context.Context, id, after uuid.UUID, limit int) ([]operationstore.Event, error) {
	var cursor any
	if after != uuid.Nil {
		cursor = after
	}
	rows, err := s.Query(ctx, `SELECT id::text,operation_id::text,state,occurred_at,sequence FROM operation_events WHERE operation_id=$1 AND sequence>COALESCE((SELECT sequence FROM operation_events WHERE id=$2 AND operation_id=$1),0) ORDER BY sequence LIMIT $3`, id, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []operationstore.Event
	for rows.Next() {
		var v operationstore.Event
		if err := rows.Scan(&v.ID, &v.OperationID, &v.State, &v.OccurredAt, &v.Sequence); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) EventSequence(ctx context.Context, operation, event uuid.UUID) (sequence int64, err error) {
	err = s.QueryRow(ctx, `SELECT sequence FROM operation_events WHERE id=$1 AND operation_id=$2`, event, operation).Scan(&sequence)
	return
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
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT c.id`+reapingSendingFrom+` ORDER BY l.leased_until,c.id LIMIT $1 FOR UPDATE OF o SKIP LOCKED`, limit))
}

func (s operationStore) LockExpiredSendingCommand(ctx context.Context, id uuid.UUID) (v operationstore.RecoveryCommand, err error) {
	v.CommandID = id
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.node_id,c.envelope,o.id,a.id,c.state,p.state,o.attempts,c.expires_at`+reapingSendingFrom+` AND c.id=$1 FOR UPDATE OF c,p`, id).
		Scan(&v.OperationID, &v.NodeID, &v.Envelope, &v.OutboxID, &v.AttemptID, &v.CommandState, &v.OperationState, &v.Attempts, &v.ExpiresAt)
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
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		ORDER BY command.created_at,command.id LIMIT $2 FOR UPDATE OF outbox SKIP LOCKED`, node, limit))
}

func (s operationStore) LockAmbiguousDispatch(ctx context.Context, command, node uuid.UUID) (v operationstore.AmbiguousDispatch, err error) {
	err = s.QueryRow(ctx, `SELECT command.operation_id,command.envelope FROM commands AS command
		JOIN operations AS operation ON operation.id=command.operation_id JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.id=$1 AND command.node_id=$2 AND command.state IN ('dispatched','accepted','running','unknown')
		AND operation.state IN ('dispatched','accepted','running','unknown')
		AND NOT (command.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.state='succeeded' AND result.receipt_verification_status='verified'))
		AND outbox.published_at IS NOT NULL AND outbox.locked_by IS NULL
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		FOR UPDATE OF command,operation`, command, node).Scan(&v.OperationID, &v.Envelope)
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

func (s operationStore) InsertIntent(ctx context.Context, v operationstore.QueuedIntent) error {
	_, err := s.Exec(ctx, `INSERT INTO operations(id,workspace_id,node_id,command_id,state,version,request_id,trace_id,idempotency_key,request_hash,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'queued',1,$5,$6,$7,$8,$9,$10,$10)`, v.ID, v.WorkspaceID, v.NodeID, v.CommandID, v.RequestID, v.TraceID, v.IdempotencyKey, v.RequestHash, v.ExpiresAt, v.CreatedAt)
	return err
}

func (s operationStore) EnqueueCommand(ctx context.Context, v operationstore.QueuedCommand) error {
	if _, err := s.Exec(ctx, `INSERT INTO commands(id,operation_id,workspace_id,node_id,state,payload_type,envelope,idempotency_key,expected_version,traceparent,expires_at,created_at,updated_at,resource_type,resource_key)
		VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)`, v.ID, v.OperationID, v.WorkspaceID, v.NodeID, v.PayloadType, v.Envelope, v.IdempotencyKey, v.ExpectedVersion, v.Traceparent, v.ExpiresAt, v.CreatedAt, v.ResourceType, v.ResourceKey); err != nil {
		return fmt.Errorf("insert command: %w", err)
	}
	if _, err := s.Exec(ctx, `INSERT INTO outbox_events(id,command_id,event_type,payload,available_at,created_at)
		VALUES($1,$2,'command.dispatch',$3,$4,$5)`, v.OutboxID, v.ID, v.Envelope, v.AvailableAt, v.CreatedAt); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'queued',$3)`, v.EventID, v.OperationID, v.CreatedAt)
	return err
}

type commandResultStore struct{ database.Tx }

func (t *transaction) CommandResultStore() resultstore.ResultStore { return commandResultStore{t} }

func (s commandResultStore) LoadCommand(ctx context.Context, command, node uuid.UUID) (v resultstore.ResultCommand, err error) {
	var outbox uuid.UUID
	err = s.QueryRow(ctx, `SELECT id FROM outbox_events WHERE command_id=$1 FOR UPDATE`, command).Scan(&outbox)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return v, err
	}
	// A separate statement establishes outbox-before-command ordering and a
	// fresh snapshot after waiting for a concurrent dispatch completion.
	err = s.QueryRow(ctx, `SELECT c.envelope,c.state,c.created_at,c.details_compacted_at IS NOT NULL,f.attempt_id IS NOT NULL,
		COALESCE(f.attempt_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(f.lease_token,'00000000-0000-0000-0000-000000000000'::uuid)
		FROM commands c JOIN operations p ON p.command_id=c.id
		LEFT JOIN LATERAL (SELECT a.id AS attempt_id,l.lease_token FROM outbox_events o
		 JOIN node_command_leases l ON l.command_id=c.id AND l.node_id=c.node_id
		 JOIN command_attempts a ON a.command_id=l.command_id AND a.outbox_event_id=o.id AND a.worker_id=l.worker_id
		 WHERE o.command_id=c.id AND l.leased_until>clock_timestamp() AND o.locked_by=l.worker_id AND o.locked_until>clock_timestamp()
		 AND a.attempt_number=o.attempts AND a.state='sending' AND a.finished_at IS NULL LIMIT 1) f ON true
		WHERE c.id=$1 AND c.node_id=$2`, command, node).Scan(&v.Envelope, &v.State, &v.CreatedAt, &v.Compacted, &v.DispatchInFlight, &v.AttemptID, &v.LeaseToken)
	return
}

func (s commandResultStore) Receipt(ctx context.Context, key string, effect []byte, sequence uint64) (id uuid.UUID, digest []byte, err error) {
	err = s.QueryRow(ctx, `SELECT command_id,receipt_sha256 FROM agent_command_results WHERE privd_attestation_key_id=$1 AND effect_record_id=$2 AND effect_sequence=$3 AND receipt_verification_status='verified'`, key, effect, sequence).Scan(&id, &digest)
	return
}

func (s commandResultStore) InsertResult(ctx context.Context, v resultstore.Result) error {
	_, err := s.Exec(ctx, `INSERT INTO agent_command_results(event_id,command_id,idempotency_key,payload_sha256,semantic_payload_hash_version,state,result,error_code,accepted_at,completed_at,replayed,created_at,receipt_verification_status,receipt_failure_reason,privd_attestation_key_id,effect_record_id,effect_sequence,receipt_sha256,privileged_result_proof)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,clock_timestamp(),$12,$13,$14,$15,$16,$17,$18)`, v.EventID, v.CommandID, v.IdempotencyKey, v.PayloadHash, v.HashVersion, v.State, v.Bytes, v.ErrorCode, v.AcceptedAt, v.CompletedAt, v.Replayed, v.VerificationStatus, v.FailureReason, v.KeyID, v.EffectRecordID, v.EffectSequence, v.ReceiptHash, v.Proof)
	return err
}

func (s commandResultStore) Workspace(ctx context.Context, node uuid.UUID) (id uuid.UUID, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id FROM nodes WHERE id=$1`, node).Scan(&id)
	return
}
func (s commandResultStore) Alert(ctx context.Context, id, workspace uuid.UUID, severity, kind string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO security_alerts(id,workspace_id,severity,kind,created_at)VALUES($1,$2,$3,$4,$5)`, id, workspace, severity, kind, at)
	return err
}
func (s commandResultStore) UpdateCommand(ctx context.Context, id uuid.UUID, state string, at value.Timestamp) (bool, error) {
	n, err := s.Exec(ctx, `UPDATE commands SET state=$2,updated_at=GREATEST(updated_at,$3) WHERE id=$1 AND state IN ('queued','dispatched','accepted','running','unknown')`, id, state, at)
	return n != 0, err
}
func (s commandResultStore) UpdateOperation(ctx context.Context, command uuid.UUID, state string, terminal bool, at value.Timestamp) (v resultstore.ResultOperation, err error) {
	err = s.QueryRow(ctx, `UPDATE operations SET state=$2,version=version+1,updated_at=GREATEST(updated_at,$3),completed_at=CASE WHEN $4::boolean THEN GREATEST(COALESCE(completed_at,$3),$3) ELSE NULL END WHERE command_id=$1 AND state IN ('queued','dispatched','accepted','running','unknown') RETURNING id,workspace_id,request_id,COALESCE(trace_id,'')`, command, state, at, terminal).Scan(&v.ID, &v.WorkspaceID, &v.RequestID, &v.TraceID)
	return
}
func (s commandResultStore) CompleteOutbox(ctx context.Context, command uuid.UUID, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),locked_by=NULL,locked_until=NULL,last_error=NULL WHERE command_id=$1`, command, at)
	return err
}
func (s commandResultStore) CloseDispatch(ctx context.Context, node, command, attempt, lease uuid.UUID) error {
	n, err := s.Exec(ctx, `UPDATE command_attempts SET state='sent',finished_at=clock_timestamp() WHERE id=$1 AND command_id=$2 AND state='sending' AND finished_at IS NULL`, attempt, command)
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `DELETE FROM node_command_leases WHERE node_id=$1 AND command_id=$2 AND lease_token=$3`, node, command, lease)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s commandResultStore) ConfigOutcome(ctx context.Context, v resultstore.ConfigOutcome) (bool, error) {
	if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state=$2,failure_code=$3,updated_at=$4 WHERE operation_id=$1`, v.OperationID, v.State, v.FailureCode, v.At); err != nil {
		return false, err
	}
	if v.State == "succeeded" {
		n, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,candidate_hash,redacted_config,automation_locked,last_apply_operation_id,updated_at)
		 SELECT x.node_id,$2,$2,$3,p.candidate_redacted,false,$1,$4 FROM config_apply_operations x JOIN config_plans p ON p.id=x.plan_id WHERE x.operation_id=$1 AND x.node_id=$5
		 ON CONFLICT(node_id) DO UPDATE SET revision=EXCLUDED.revision,desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),candidate_hash=EXCLUDED.candidate_hash,redacted_config=EXCLUDED.redacted_config,automation_locked=false,automation_lock_reason=NULL,last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, v.OperationID, v.Revision, v.CandidateHash, v.At, v.NodeID)
		return n == 1, err
	}
	if v.State == "failed_critical" {
		if _, err := s.Exec(ctx, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,automation_locked,automation_lock_reason,last_apply_operation_id,updated_at)
		 VALUES($1,0,$4,'',true,'config_apply_rollback_failed',$2,$3) ON CONFLICT(node_id) DO UPDATE SET desired_revision=GREATEST(node_config_state.desired_revision,EXCLUDED.desired_revision),automation_locked=true,automation_lock_reason='config_apply_rollback_failed',last_apply_operation_id=EXCLUDED.last_apply_operation_id,updated_at=EXCLUDED.updated_at`, v.NodeID, v.OperationID, v.At, v.Revision); err != nil {
			return false, err
		}
		return true, s.Alert(ctx, v.AlertID, v.WorkspaceID, "critical", "config_apply.rollback_failed", v.At)
	}
	return true, nil
}
func (s commandResultStore) CSROutcome(ctx context.Context, v resultstore.CSROutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,csr_der=COALESCE($3,csr_der),public_key_sha256=COALESCE($4,public_key_sha256),csr_receipt_verified_at=COALESCE($7,csr_receipt_verified_at),csr_receipt_sha256=COALESCE($8,csr_receipt_sha256),csr_privd_attestation_key_id=COALESCE($9,csr_privd_attestation_key_id),csr_effect_record_id=COALESCE($10,csr_effect_record_id),csr_der_sha256=COALESCE($11,csr_der_sha256),csr_requested_subject_sha256=COALESCE($12,csr_requested_subject_sha256),updated_at=$5 WHERE id=$1 AND operation_id=$6`, v.CertificateID, v.State, v.DER, v.PublicHash, v.At, v.OperationID, v.VerifiedAt, v.ReceiptHash, v.KeyID, v.EffectRecordID, v.CSRHash, v.SubjectHash)
	return err
}
func (s commandResultStore) RevokeOutcome(ctx context.Context, v resultstore.RevokeOutcome) error {
	_, err := s.Exec(ctx, `UPDATE certificates SET state=$2,version=version+1,revoked_at=COALESCE($3,revoked_at),revocation_reason=CASE WHEN $2='revoked' THEN $4 ELSE revocation_reason END,updated_at=$5 WHERE id=$1 AND node_id=$6`, v.CertificateID, v.State, v.RevokedAt, v.Reason, v.At, v.NodeID)
	return err
}
func (s commandResultStore) ArtifactOutcome(ctx context.Context, v resultstore.ArtifactOutcome) error {
	_, err := s.Exec(ctx, `UPDATE artifact_operations SET state=$2,content_sha256=COALESCE($3,content_sha256),content_size=COALESCE($4,content_size),updated_at=$5 WHERE id=$1 AND operation_id=$6`, v.ArtifactID, v.State, v.Digest, v.Size, v.At, v.OperationID)
	return err
}
func (s commandResultStore) UpgradeOutcome(ctx context.Context, operation uuid.UUID, state string, scheduled, at value.Timestamp) error {
	_, err := s.Exec(ctx, `UPDATE agent_upgrade_operations SET state=$2,scheduled_at=COALESCE($3,scheduled_at),updated_at=$4 WHERE operation_id=$1`, operation, state, scheduled, at)
	return err
}
func (s commandResultStore) AppendEvent(ctx context.Context, id, operation uuid.UUID, state string, at value.Timestamp) error {
	_, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,$3,$4)`, id, operation, state, at)
	return err
}
func (s commandResultStore) ScheduleRecovery(ctx context.Context, command uuid.UUID, payload []byte, expires, at value.Timestamp) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,expires_at=$3 WHERE id=$1 AND state='unknown'`, command, payload, expires); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE operations SET expires_at=$2 WHERE command_id=$1 AND state='unknown'`, command, expires); err != nil {
		return err
	}
	_, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=$3,last_error=NULL WHERE command_id=$1`, command, payload, at)
	return err
}

type commandLimitStore struct{ tx database.Tx }

func (t *transaction) CommandLimitStore() commandlimit.Store { return &commandLimitStore{t} }

func (s *commandLimitStore) LockAdmission(ctx context.Context) error {
	_, err := s.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, commandlimit.AdmissionLockID)
	return err
}

func (s *commandLimitStore) LockBacklog(ctx context.Context) error {
	_, err := s.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, commandlimit.BacklogLockID)
	return err
}

// Active counts executing commands. An executable recovery retry moves only its
// command out of Unknown, so it counts by command state until a result moves
// the operation; a quiescent Unknown counts only while it holds a lease.
func (s *commandLimitStore) Active(ctx context.Context, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT operation.id FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running') OR (operation.state='unknown' AND command.state IN('dispatched','accepted','running')) UNION SELECT command.operation_id FROM node_command_leases lease JOIN commands command ON command.id=lease.command_id LIMIT $1) active`, limit).Scan(&count)
	return count, err
}

func (s *commandLimitStore) NodeBacklog(ctx context.Context, node uuid.UUID, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE node_id=$1 AND state IN('queued','offline_pending','dispatched','accepted','running','unknown') LIMIT $2) backlog`, node, limit).Scan(&count)
	return count, err
}

func (s *commandLimitStore) WorkspaceBacklog(ctx context.Context, workspace uuid.UUID, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE workspace_id=$1 AND state IN('queued','offline_pending','dispatched','accepted','running','unknown') LIMIT $2) backlog`, workspace, limit).Scan(&count)
	return count, err
}

var _ commandlimit.Store = (*commandLimitStore)(nil)
