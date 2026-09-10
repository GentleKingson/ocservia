package postgres

import (
	"context"
	"errors"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/google/uuid"
)

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
