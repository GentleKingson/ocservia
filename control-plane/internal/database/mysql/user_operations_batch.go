package mysql

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

func (s userOperationsStore) AcquireLease(ctx context.Context, name string, owner uuid.UUID, duration time.Duration) (bool, error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return false, err
	}
	until, err := now.Add(duration)
	if err != nil {
		return false, err
	}
	_, err = s.Exec(ctx, `INSERT INTO scheduler_leases(lease_name,owner_id,lease_until,updated_at)VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE lease_name=VALUES(lease_name)`, name, UUIDBytes(owner), until, now)
	if err != nil {
		return false, err
	}
	var heldBy uuid.UUID
	var heldUntil value.Timestamp
	if err := s.QueryRow(ctx, `SELECT owner_id,lease_until FROM scheduler_leases WHERE BINARY lease_name=? FOR UPDATE`, name).Scan(&heldBy, &heldUntil); err != nil {
		return false, err
	}
	if heldBy != owner && heldUntil.Micros > now.Micros {
		return false, nil
	}
	_, err = s.Exec(ctx, `UPDATE scheduler_leases SET owner_id=?,lease_until=?,updated_at=? WHERE BINARY lease_name=?`, UUIDBytes(owner), until, now, name)
	return err == nil, err
}

func (s userOperationsStore) ClaimBatchItems(ctx context.Context, owner uuid.UUID, limit int) ([]userstore.BatchItem, error) {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return nil, err
	}
	until, err := now.Add(20 * time.Second)
	if err != nil {
		return nil, err
	}
	rows, err := s.Query(ctx, `SELECT batch_id,item_index,node_id,username,action,expected_version FROM batch_operation_items WHERE (state='queued' AND lease_until IS NULL) OR (state='submitting' AND lease_until<=?) ORDER BY updated_at,batch_id,item_index LIMIT ? FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	var items []userstore.BatchItem
	for rows.Next() {
		var v userstore.BatchItem
		if err := rows.Scan(&v.BatchID, &v.Index, &v.NodeID, &v.Username, &v.Action, &v.ExpectedVersion); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, v := range items {
		if _, err := s.Exec(ctx, `UPDATE batch_operation_items SET state='submitting',lease_owner=?,lease_until=?,updated_at=? WHERE batch_id=? AND item_index=?`, UUIDBytes(owner), until, now, UUIDBytes(v.BatchID), v.Index); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s userOperationsStore) ReleaseBatchClaims(ctx context.Context, owner uuid.UUID) error {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE batch_operation_items SET state='queued',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE lease_owner=? AND state='submitting'`, now, UUIDBytes(owner))
	return err
}

func (s userOperationsStore) FinishBatchItem(ctx context.Context, v userstore.BatchItem, owner uuid.UUID, operation *uuid.UUID, errorType string) error {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	if operation == nil {
		_, err = s.Exec(ctx, `UPDATE batch_operation_items SET state='failed',error_type=?,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE batch_id=? AND item_index=? AND lease_owner=?`, errorType, now, UUIDBytes(v.BatchID), v.Index, UUIDBytes(owner))
		return err
	}
	_, err = s.Exec(ctx, `UPDATE batch_operation_items SET state='submitted',child_operation_id=?,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE batch_id=? AND item_index=? AND lease_owner=?`, UUIDBytes(*operation), now, UUIDBytes(v.BatchID), v.Index, UUIDBytes(owner))
	return err
}

func (s userOperationsStore) RefreshBatchItems(ctx context.Context, limit int) error {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, `UPDATE batch_operation_items item JOIN (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT ?) active ON active.id=item.batch_id JOIN operations op ON op.id=item.child_operation_id JOIN nodes node ON node.id=op.node_id SET item.state=CASE WHEN op.state='queued' AND node.status='offline' THEN 'offline_pending' WHEN op.state='succeeded' THEN 'succeeded' WHEN op.state IN('failed','expired','rolled_back') THEN 'failed' WHEN op.state='unknown' THEN 'unknown' WHEN op.state='offline_pending' THEN 'offline_pending' ELSE item.state END,item.error_type=CASE WHEN op.state IN('failed','expired','rolled_back') THEN op.state ELSE item.error_type END,item.updated_at=? WHERE item.state IN('submitted','unknown','offline_pending')`, limit, now)
	return err
}

func (s userOperationsStore) RefreshBatches(ctx context.Context, limit int) error {
	now, err := database.TransactionTime(ctx, s.Tx)
	if err != nil {
		return err
	}
	// LIMIT and GROUP BY materialize both derived tables on either engine,
	// avoiding a target-table read in the outer UPDATE.
	_, err = s.Exec(ctx, `UPDATE batch_operations batch JOIN (SELECT item.batch_id,CASE WHEN MAX(item.state IN('queued','submitting','submitted','unknown','offline_pending')) THEN CASE WHEN MAX(item.state IN('failed','forbidden')) THEN 'partial_failed' ELSE 'running' END WHEN MIN(item.state='succeeded') THEN 'succeeded' WHEN MAX(item.state='succeeded') THEN 'partial_failed' ELSE 'failed' END state FROM batch_operation_items item JOIN (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT ?) active ON active.id=item.batch_id GROUP BY item.batch_id) summary ON batch.id=summary.batch_id SET batch.state=summary.state,batch.updated_at=?`, limit, now)
	return err
}
