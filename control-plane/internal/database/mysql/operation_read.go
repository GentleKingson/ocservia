package mysql

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

const operationColumns = `o.id,o.state,o.node_id,o.command_id,o.version,o.created_at,o.updated_at,o.expires_at,COALESCE(x.state,''),COALESCE(x.failure_code,''),COALESCE(u.state,''),COALESCE(u.target_version,'')`
const operationJoins = ` FROM operations o LEFT JOIN config_apply_operations x ON x.operation_id=o.id LEFT JOIN agent_upgrade_operations u ON u.operation_id=o.id`

func scanOperation(row database.Row, same *bool) (v operationstore.Operation, err error) {
	var id uuid.UUID
	var node, command *uuid.UUID
	var expires value.Timestamp
	args := []any{&id, &v.State, &node, &command, &v.Version, &v.CreatedAt, &v.UpdatedAt, &expires, &v.ConfigApplyState, &v.ConfigApplyFailureCode, &v.AgentUpgradeState, &v.AgentUpgradeTarget}
	if same != nil {
		args = append(args, same)
	}
	err = row.Scan(args...)
	if err != nil {
		return operationstore.Operation{}, err
	}
	v.ID = id.String()
	if node != nil {
		text := node.String()
		v.NodeID = &text
	}
	if command != nil {
		text := command.String()
		v.CommandID = &text
	}
	if expires.Valid {
		v.ExpiresAt = &expires
	}
	return
}

func (s operationStore) Get(ctx context.Context, id uuid.UUID) (operationstore.Operation, error) {
	return scanOperation(s.QueryRow(ctx, `SELECT `+operationColumns+operationJoins+` WHERE o.id=?`, UUIDBytes(id)), nil)
}

func (s operationStore) ListInWorkspace(ctx context.Context, workspace, after uuid.UUID, limit int) ([]operationstore.Operation, error) {
	var workspaceID, cursor any
	if workspace != uuid.Nil {
		workspaceID = UUIDBytes(workspace)
	}
	if after != uuid.Nil {
		cursor = UUIDBytes(after)
	}
	rows, err := s.Query(ctx, `SELECT `+operationColumns+operationJoins+` WHERE (? IS NULL OR o.id<?) AND (? IS NULL OR o.workspace_id=?) ORDER BY o.id DESC LIMIT ?`, cursor, cursor, workspaceID, workspaceID, limit)
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
		workspaceID = UUIDBytes(workspace)
	}
	err = s.QueryRow(ctx, `SELECT count(CASE WHEN state NOT IN ('succeeded','failed','expired','rolled_back','drifted','superseded','unknown') THEN 1 END),count(CASE WHEN state='unknown' THEN 1 END) FROM operations WHERE (? IS NULL OR workspace_id=?)`, workspaceID, workspaceID).Scan(&v.Active, &v.Unknown)
	return
}

func (s operationStore) FindIdempotent(ctx context.Context, workspace uuid.UUID, key string, hash []byte) (v operationstore.Operation, same bool, err error) {
	v, err = scanOperation(s.QueryRow(ctx, `SELECT `+operationColumns+`,o.request_hash=?`+operationJoins+` WHERE o.workspace_id=? AND BINARY o.idempotency_key=?`, hash, UUIDBytes(workspace), key), &same)
	if errors.Is(err, database.ErrNotFound) {
		return operationstore.Operation{}, false, nil
	}
	return
}

func (s operationStore) Events(ctx context.Context, id, after uuid.UUID, limit int) ([]operationstore.Event, error) {
	var cursor any
	if after != uuid.Nil {
		cursor = UUIDBytes(after)
	}
	rows, err := s.Query(ctx, `SELECT id,operation_id,state,occurred_at,sequence FROM operation_events WHERE operation_id=? AND sequence>COALESCE((SELECT sequence FROM operation_events WHERE id=? AND operation_id=?),0) ORDER BY sequence LIMIT ?`, UUIDBytes(id), cursor, UUIDBytes(id), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []operationstore.Event
	for rows.Next() {
		var v operationstore.Event
		var event, operation uuid.UUID
		if err := rows.Scan(&event, &operation, &v.State, &v.OccurredAt, &v.Sequence); err != nil {
			return nil, err
		}
		v.ID, v.OperationID = event.String(), operation.String()
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s operationStore) EventSequence(ctx context.Context, operation, event uuid.UUID) (sequence int64, err error) {
	err = s.QueryRow(ctx, `SELECT sequence FROM operation_events WHERE id=? AND operation_id=?`, UUIDBytes(event), UUIDBytes(operation)).Scan(&sequence)
	return
}
