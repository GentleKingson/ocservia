package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"math"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

type connectionOwnerStore struct{ *transaction }

func (t *transaction) ConnectionOwnerStore() connectionowner.Store { return connectionOwnerStore{t} }

func assertConnectionOwner(ctx context.Context, s store, node, connection []byte, epoch int64) error {
	var one int
	return s.QueryRow(ctx, `SELECT 1 FROM connection_owner_fencing WHERE node_id=$1 AND connection_id=$2 AND session_epoch=$3 AND active=true FOR SHARE OF connection_owner_fencing`,
		node, connection, epoch).Scan(&one)
}

func (s connectionOwnerStore) Acquire(ctx context.Context, nodeID, connectionID [16]byte) (int64, error) {
	if _, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "connection-owner:"+hex.EncodeToString(nodeID[:])); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.QueryRow(ctx, `SELECT session_epoch FROM connection_owner_fencing WHERE node_id=$1 FOR UPDATE`, nodeID[:]).Scan(&epoch)
	absent := errors.Is(err, database.ErrNotFound)
	if err != nil && !absent {
		return 0, err
	}
	if absent {
		_, err = s.Exec(ctx, `INSERT INTO connection_owner_fencing(node_id,connection_id,session_epoch,active,updated_at) VALUES($1,$2,1,true,now())`, nodeID[:], connectionID[:])
		return 1, err
	}
	if epoch == math.MaxInt64 {
		return 0, database.ErrConstraint
	}
	next := epoch + 1
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET connection_id=$2,session_epoch=$3,active=true,updated_at=now() WHERE node_id=$1 AND session_epoch=$4`,
		nodeID[:], connectionID[:], next, epoch)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	return next, err
}

func (s connectionOwnerStore) Release(ctx context.Context, nodeID, connectionID [16]byte, epoch int64) error {
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET active=false,updated_at=now() WHERE node_id=$1 AND connection_id=$2 AND session_epoch=$3 AND active=true`,
		nodeID[:], connectionID[:], epoch)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s connectionOwnerStore) Assert(ctx context.Context, nodeID, connectionID [16]byte, epoch int64) error {
	return assertConnectionOwner(ctx, s.store, nodeID[:], connectionID[:], epoch)
}

func (s connectionOwnerStore) Read(ctx context.Context, nodeID [16]byte) (v connectionowner.OwnerState, err error) {
	var connection []byte
	err = s.QueryRow(ctx, `SELECT connection_id,session_epoch,active FROM connection_owner_fencing WHERE node_id=$1`, nodeID[:]).
		Scan(&connection, &v.Epoch, &v.Active)
	copy(v.ConnectionID[:], connection)
	return
}
