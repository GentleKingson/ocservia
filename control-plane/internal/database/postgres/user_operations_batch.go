package postgres

import (
	"context"
	"time"

	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
)

func (s userOperationsStore) AcquireLease(ctx context.Context, name string, owner uuid.UUID, duration time.Duration) (bool, error) {
	n, err := s.Exec(ctx, `INSERT INTO scheduler_leases(lease_name,owner_id,lease_until,updated_at)VALUES($1,$2,now()+$3::interval,now()) ON CONFLICT(lease_name) DO UPDATE SET owner_id=EXCLUDED.owner_id,lease_until=EXCLUDED.lease_until,updated_at=EXCLUDED.updated_at WHERE scheduler_leases.lease_until<=now() OR scheduler_leases.owner_id=EXCLUDED.owner_id`, name, owner, duration.String())
	return n == 1, err
}

func (s userOperationsStore) ClaimBatchItems(ctx context.Context, owner uuid.UUID, limit int) ([]userstore.BatchItem, error) {
	rows, err := s.Query(ctx, `WITH claim AS (SELECT batch_id,item_index FROM batch_operation_items WHERE (state='queued' AND lease_until IS NULL) OR (state='submitting' AND lease_until<=now()) ORDER BY updated_at,batch_id,item_index FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE batch_operation_items item SET state='submitting',lease_owner=$2,lease_until=now()+interval '20 seconds',updated_at=now() FROM claim WHERE item.batch_id=claim.batch_id AND item.item_index=claim.item_index RETURNING item.batch_id,item.item_index,item.node_id,item.username,item.action,item.expected_version`, limit, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []userstore.BatchItem
	for rows.Next() {
		var v userstore.BatchItem
		if err := rows.Scan(&v.BatchID, &v.Index, &v.NodeID, &v.Username, &v.Action, &v.ExpectedVersion); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s userOperationsStore) ReleaseBatchClaims(ctx context.Context, owner uuid.UUID) error {
	_, err := s.Exec(ctx, `UPDATE batch_operation_items SET state='queued',lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE lease_owner=$1 AND state='submitting'`, owner)
	return err
}

func (s userOperationsStore) FinishBatchItem(ctx context.Context, v userstore.BatchItem, owner uuid.UUID, operation *uuid.UUID, errorType string) error {
	if operation == nil {
		_, err := s.Exec(ctx, `UPDATE batch_operation_items SET state='failed',error_type=$4,lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE batch_id=$1 AND item_index=$2 AND lease_owner=$3`, v.BatchID, v.Index, owner, errorType)
		return err
	}
	_, err := s.Exec(ctx, `UPDATE batch_operation_items SET state='submitted',child_operation_id=$4,lease_owner=NULL,lease_until=NULL,updated_at=now() WHERE batch_id=$1 AND item_index=$2 AND lease_owner=$3`, v.BatchID, v.Index, owner, *operation)
	return err
}

func (s userOperationsStore) RefreshBatchItems(ctx context.Context, limit int) error {
	_, err := s.Exec(ctx, `WITH active AS (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT $1) UPDATE batch_operation_items item SET state=CASE WHEN op.state='queued' AND node.status='offline' THEN 'offline_pending' WHEN op.state='succeeded' THEN 'succeeded' WHEN op.state IN('failed','expired','rolled_back') THEN 'failed' WHEN op.state='unknown' THEN 'unknown' WHEN op.state='offline_pending' THEN 'offline_pending' ELSE item.state END,error_type=CASE WHEN op.state IN('failed','expired','rolled_back') THEN op.state ELSE item.error_type END,updated_at=now() FROM operations op JOIN nodes node ON node.id=op.node_id,active WHERE item.batch_id=active.id AND item.child_operation_id=op.id AND item.state IN('submitted','unknown','offline_pending')`, limit)
	return err
}

func (s userOperationsStore) RefreshBatches(ctx context.Context, limit int) error {
	_, err := s.Exec(ctx, `WITH active AS (SELECT id FROM batch_operations WHERE state IN('queued','running','partial_failed') ORDER BY updated_at,id LIMIT $1),summary AS (SELECT item.batch_id,CASE WHEN bool_or(item.state IN('queued','submitting','submitted','unknown','offline_pending')) THEN CASE WHEN bool_or(item.state IN('failed','forbidden')) THEN 'partial_failed' ELSE 'running' END WHEN bool_and(item.state='succeeded') THEN 'succeeded' WHEN bool_or(item.state='succeeded') THEN 'partial_failed' ELSE 'failed' END state FROM batch_operation_items item JOIN active ON active.id=item.batch_id GROUP BY item.batch_id) UPDATE batch_operations batch SET state=summary.state,updated_at=now() FROM summary WHERE batch.id=summary.batch_id`, limit)
	return err
}
