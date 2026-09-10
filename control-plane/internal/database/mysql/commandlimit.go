package mysql

import (
	"context"
	"strconv"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/google/uuid"
)

type commandLimitStore struct{ tx database.Tx }

func (t *transaction) CommandLimitStore() commandlimit.Store { return &commandLimitStore{t} }

func (s *commandLimitStore) LockAdmission(ctx context.Context) error {
	return LockTransaction(ctx, s.tx, "pg-advisory:"+strconv.FormatInt(commandlimit.AdmissionLockID, 10))
}

func (s *commandLimitStore) LockBacklog(ctx context.Context) error {
	return LockTransaction(ctx, s.tx, "pg-advisory:"+strconv.FormatInt(commandlimit.BacklogLockID, 10))
}

func (s *commandLimitStore) Active(ctx context.Context, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT operation.id FROM operations operation JOIN commands command ON command.operation_id=operation.id WHERE operation.state IN('dispatched','accepted','running','unknown') UNION SELECT command.operation_id FROM node_command_leases lease JOIN commands command ON command.id=lease.command_id LIMIT ?) active`, limit).Scan(&count)
	return count, err
}

func (s *commandLimitStore) NodeBacklog(ctx context.Context, node uuid.UUID, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE node_id=? AND state IN('queued','offline_pending') LIMIT ?) backlog`, UUIDBytes(node), limit).Scan(&count)
	return count, err
}

func (s *commandLimitStore) WorkspaceBacklog(ctx context.Context, workspace uuid.UUID, limit int) (int, error) {
	var count int
	err := s.tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM operations WHERE workspace_id=? AND state IN('queued','offline_pending') LIMIT ?) backlog`, UUIDBytes(workspace), limit).Scan(&count)
	return count, err
}

var _ commandlimit.Store = (*commandLimitStore)(nil)
