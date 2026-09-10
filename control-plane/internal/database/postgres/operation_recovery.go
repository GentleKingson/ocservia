package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) ReconnectAuthority(ctx context.Context, node, connection uuid.UUID, epoch int64) (v operationstore.RecoveryAuthority, err error) {
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation FROM connection_owner_fencing
		WHERE node_id=$1 AND connection_id=$2 AND owner_epoch=$3 AND lease_until>clock_timestamp()
		FOR SHARE OF connection_owner_fencing`, node[:], connection[:], epoch).Scan(&v.OwnerID, &v.Incarnation)
	return
}

func (s operationStore) AmbiguousDispatches(ctx context.Context, node uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.Query(ctx, `SELECT command.id FROM commands AS command JOIN operations AS operation ON operation.id=command.operation_id
		JOIN outbox_events AS outbox ON outbox.command_id=command.id
		WHERE command.node_id=$1 AND command.state IN ('dispatched','accepted','running','unknown')
		AND operation.state IN ('dispatched','accepted','running','unknown')
		AND NOT (command.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results AS result WHERE result.command_id=command.id AND result.state='succeeded' AND result.receipt_verification_status='verified'))
		AND outbox.published_at IS NOT NULL AND outbox.locked_by IS NULL
		AND NOT EXISTS(SELECT 1 FROM node_command_leases AS lease WHERE lease.command_id=command.id)
		ORDER BY command.created_at,command.id LIMIT $2 FOR UPDATE OF outbox SKIP LOCKED`, node, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
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
