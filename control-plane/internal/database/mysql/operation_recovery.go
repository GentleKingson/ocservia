package mysql

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

func (s operationStore) ReconnectAuthority(ctx context.Context, node, connection uuid.UUID, epoch int64) (v operationstore.RecoveryAuthority, err error) {
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation FROM connection_owner_fencing WHERE node_id=? AND connection_id=? AND owner_epoch=?`, UUIDBytes(node), UUIDBytes(connection), epoch).Scan(&v.OwnerID, &v.Incarnation)
	if err != nil {
		return v, err
	}
	err = (connectionOwnerStore{s.Tx}).Assert(ctx, ownerstore.Term{
		Identity: ownerstore.Identity{InstanceID: v.OwnerID, Incarnation: v.Incarnation},
		NodeID:   node, ConnectionID: connection, Epoch: epoch,
	})
	return
}

func (s operationStore) AmbiguousDispatches(ctx context.Context, node uuid.UUID, limit int) ([]uuid.UUID, error) {
	// Lock only outbox rows. In particular, do not take command locks ahead
	// of the outbox locks held by concurrent result ingestion.
	rows, err := s.Query(ctx, `SELECT o.command_id FROM outbox_events o WHERE o.published_at IS NOT NULL AND o.locked_by IS NULL
		AND EXISTS(SELECT 1 FROM commands c JOIN operations p ON p.id=c.operation_id
		 WHERE c.id=o.command_id AND c.node_id=? AND c.state IN ('dispatched','accepted','running','unknown')
		 AND p.state IN ('dispatched','accepted','running','unknown')
		 AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
		 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id))
		ORDER BY (SELECT c.created_at FROM commands c WHERE c.id=o.command_id),o.command_id LIMIT ? FOR UPDATE SKIP LOCKED`, UUIDBytes(node), limit)
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
	// The outbox is already locked. Current locking reads see any committed
	// result instead of the earlier candidate-selection snapshot.
	err = s.QueryRow(ctx, `SELECT c.operation_id,c.envelope FROM commands c JOIN operations p ON p.id=c.operation_id JOIN outbox_events o ON o.command_id=c.id
		WHERE c.id=? AND c.node_id=? AND c.state IN ('dispatched','accepted','running','unknown')
		AND p.state IN ('dispatched','accepted','running','unknown')
		AND NOT(c.payload_type='agent_upgrade' AND EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='succeeded' AND r.receipt_verification_status='verified'))
		AND o.published_at IS NOT NULL AND o.locked_by IS NULL
		AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id) FOR UPDATE`, UUIDBytes(command), UUIDBytes(node)).Scan(&v.OperationID, &v.Envelope)
	return
}

func (s operationStore) UpgradeSchedulingAcked(ctx context.Context, operation uuid.UUID) (acked bool, err error) {
	err = s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands c JOIN agent_command_results r ON r.command_id=c.id
		WHERE c.operation_id=? AND c.payload_type='agent_upgrade' AND r.state='succeeded' AND r.receipt_verification_status='verified')`, UUIDBytes(operation)).Scan(&acked)
	return
}

func (s operationStore) MarkRecoveryProjection(ctx context.Context, v operationstore.RecoveryProjection) error {
	if v.ConfigApply {
		if _, err := s.Exec(ctx, `UPDATE config_apply_operations SET state='unknown',updated_at=? WHERE operation_id=? AND state IN ('queued','dispatched','accepted','running','unknown')`, v.At, UUIDBytes(v.OperationID)); err != nil {
			return err
		}
	}
	if v.CertificateID != uuid.Nil {
		if _, err := s.Exec(ctx, `UPDATE certificates SET state='unknown',version=version+1,updated_at=? WHERE id=? AND state NOT IN ('issued','expired','revoked','failed')`, v.At, UUIDBytes(v.CertificateID)); err != nil {
			return err
		}
	}
	if v.Artifact {
		if _, err := s.Exec(ctx, `UPDATE artifact_operations SET state='pending',updated_at=? WHERE operation_id=? AND state IN ('pending','leased')`, v.At, UUIDBytes(v.OperationID)); err != nil {
			return err
		}
	}
	return nil
}

func (s operationStore) ScheduleReconnectRecovery(ctx context.Context, command, operation uuid.UUID, payload []byte, expires, at value.Timestamp) error {
	n, err := s.Exec(ctx, `UPDATE commands SET state='unknown',envelope=?,expires_at=?,updated_at=? WHERE id=? AND state IN ('dispatched','accepted','running','unknown')`, payload, expires, at, UUIDBytes(command))
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE operations SET state='unknown',version=version+1,expires_at=?,updated_at=?,completed_at=NULL WHERE id=? AND state IN ('dispatched','accepted','running','unknown')`, expires, at, UUIDBytes(operation))
	if err != nil {
		return err
	}
	if n != 1 {
		return database.ErrNotFound
	}
	n, err = s.Exec(ctx, `UPDATE outbox_events SET payload=?,published_at=NULL,locked_by=NULL,locked_until=NULL,available_at=?,last_error='owner connection changed; reconciliation required'
		WHERE command_id=? AND published_at IS NOT NULL AND locked_by IS NULL`, payload, at, UUIDBytes(command))
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
	_, err = s.Exec(ctx, `INSERT INTO operation_events(id,operation_id,state,occurred_at)VALUES(?,?,'unknown',?)`, UUIDBytes(id), UUIDBytes(operation), at)
	return err
}
