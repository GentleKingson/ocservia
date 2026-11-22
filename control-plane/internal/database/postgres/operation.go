package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

type operationStore struct{ database.Tx }

func (t *transaction) OperationStore() operationstore.Store { return operationStore{t} }

func (s operationStore) LockNode(ctx context.Context, id uuid.UUID) (v operationstore.NodeState, err error) {
	err = s.QueryRow(ctx, `SELECT workspace_id,version,authorization_revision,status FROM nodes WHERE id=$1 FOR NO KEY UPDATE`, id).Scan(&v.WorkspaceID, &v.Version, &v.AuthorizationRevision, &v.Status)
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

func (s operationStore) QueueMetrics(ctx context.Context, workspaceID uuid.UUID, all bool) (v operationstore.QueueMetrics, err error) {
	if all {
		err = s.QueryRow(ctx, `SELECT count(*),COALESCE(extract(epoch FROM now()-min(o.created_at)),0),
			(SELECT count(*) FROM commands WHERE state='queued'),
			(SELECT count(*) FROM commands WHERE state='unknown'),
			(SELECT count(*) FROM config_apply_operations WHERE state='rolled_back'),
			(SELECT count(*) FROM config_apply_operations WHERE state='failed_critical')
			FROM outbox_events o WHERE o.published_at IS NULL`).
			Scan(&v.Unpublished, &v.OldestAge, &v.Queued, &v.Unknown, &v.ConfigRollbacks, &v.ConfigFailedCritical)
		return
	}
	err = s.QueryRow(ctx, `SELECT count(*),COALESCE(extract(epoch FROM now()-min(o.created_at)),0),
		(SELECT count(*) FROM commands WHERE state='queued' AND workspace_id=$1),
		(SELECT count(*) FROM commands WHERE state='unknown' AND workspace_id=$1),
		(SELECT count(*) FROM config_apply_operations WHERE state='rolled_back' AND workspace_id=$1),
		(SELECT count(*) FROM config_apply_operations WHERE state='failed_critical' AND workspace_id=$1)
		FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE o.published_at IS NULL AND c.workspace_id=$1`, workspaceID).
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
	if err != nil || len(ids) == 0 {
		return err
	}
	eventIDs := make([]uuid.UUID, len(ids))
	for i := range ids {
		eventIDs[i], err = uuid.NewV7()
		if err != nil {
			return err
		}
	}
	if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at) SELECT u.id,u.operation_id,'expired',now() FROM unnest($1::uuid[],$2::uuid[]) AS u(id,operation_id)`, eventIDs, ids); err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE config_apply_operations SET state='expired',updated_at=now() WHERE operation_id=ANY($1::uuid[]) AND state='queued'`, ids)
	return err
}

const operationColumns = `o.id::text,o.state,o.node_id::text,o.command_id::text,o.version,o.created_at,o.updated_at,o.expires_at,COALESCE(x.state,''),COALESCE(x.failure_code,''),COALESCE(u.state,''),COALESCE(u.target_version,'')`
const operationJoins = ` FROM operations o LEFT JOIN config_apply_operations x ON x.operation_id=o.id LEFT JOIN agent_upgrade_operations u ON u.operation_id=o.id`

func scanOperation(row database.Row, same *bool) (v operationstore.Operation, err error) {
	args := []any{&v.ID, &v.State, &v.NodeID, &v.CommandID, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.ExpiresAt, &v.ConfigApplyState, &v.ConfigApplyFailureCode, &v.AgentUpgradeState, &v.AgentUpgradeTarget}
	if same != nil {
		args = append(args, same)
	}
	if err = row.Scan(args...); err != nil {
		return operationstore.Operation{}, err
	}
	return
}

func (s operationStore) Get(ctx context.Context, id uuid.UUID) (operationstore.Operation, error) {
	return scanOperation(s.QueryRow(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.id=$1`, id), nil)
}

func (s operationStore) OperationState(ctx context.Context, id uuid.UUID) (state string, err error) {
	err = s.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, id).Scan(&state)
	return
}

func (s operationStore) ListInWorkspace(ctx context.Context, workspace, after uuid.UUID, limit int) ([]operationstore.Operation, error) {
	var (
		rows database.Rows
		err  error
	)
	switch {
	case workspace == uuid.Nil && after == uuid.Nil:
		rows, err = s.Query(ctx, `SELECT `+operationColumns+operationJoins+` ORDER BY o.id DESC LIMIT $1`, limit)
	case workspace == uuid.Nil:
		rows, err = s.Query(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.id<$1 ORDER BY o.id DESC LIMIT $2`, after, limit)
	case after == uuid.Nil:
		rows, err = s.Query(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.workspace_id=$1 ORDER BY o.id DESC LIMIT $2`, workspace, limit)
	default:
		rows, err = s.Query(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.workspace_id=$1 AND o.id<$2 ORDER BY o.id DESC LIMIT $3`, workspace, after, limit)
	}
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
	if workspace == uuid.Nil {
		err = s.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state NOT IN ('succeeded','failed','expired','rolled_back','drifted','superseded','unknown')),count(*) FILTER (WHERE state='unknown') FROM operations`).Scan(&v.Active, &v.Unknown)
		return
	}
	err = s.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state NOT IN ('succeeded','failed','expired','rolled_back','drifted','superseded','unknown')),count(*) FILTER (WHERE state='unknown') FROM operations WHERE workspace_id=$1`, workspace).Scan(&v.Active, &v.Unknown)
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
	var (
		rows database.Rows
		err  error
	)
	if after == uuid.Nil {
		rows, err = s.Query(ctx, `SELECT id::text,operation_id::text,state,occurred_at,sequence FROM operation_events WHERE operation_id=$1 ORDER BY sequence LIMIT $2`, id, limit)
	} else {
		rows, err = s.Query(ctx, `SELECT id::text,operation_id::text,state,occurred_at,sequence FROM operation_events WHERE operation_id=$1 AND sequence>COALESCE((SELECT sequence FROM operation_events WHERE id=$2 AND operation_id=$1),0) ORDER BY sequence LIMIT $3`, id, after, limit)
	}
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

func (s operationStore) SupersedePending(ctx context.Context, node uuid.UUID, payload string, at time.Time) error {
	// Dispatch and command result ingestion lock outbox_events before commands.
	// Lock outbox first and recheck eligibility after acquiring the lock so a
	// concurrent dispatch claim cannot deadlock or be superseded in flight.
	commands, err := database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT o.command_id FROM outbox_events o
		WHERE o.published_at IS NULL AND o.locked_by IS NULL
		AND EXISTS(SELECT 1 FROM commands c WHERE c.id=o.command_id AND c.node_id=$1 AND c.payload_type=$2 AND c.state='queued'
			AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id))
		ORDER BY o.id FOR UPDATE`, node, payload))
	if err != nil {
		return err
	}
	for _, command := range commands {
		var operation uuid.UUID
		err := s.QueryRow(ctx, `SELECT operation_id FROM commands WHERE id=$1 AND state='queued' AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=commands.id) FOR UPDATE`, command).Scan(&operation)
		if errors.Is(err, database.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE commands SET state='superseded',updated_at=$2 WHERE id=$1`, command, at); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE operations SET state='superseded',version=version+1,updated_at=$2,completed_at=$2 WHERE id=$1 AND state='queued'`, operation, at); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE outbox_events SET published_at=$2,last_error='superseded by newer intent' WHERE command_id=$1`, command, at); err != nil {
			return err
		}
		event, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES($1,$2,'superseded',$3)`, event, operation, at); err != nil {
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
