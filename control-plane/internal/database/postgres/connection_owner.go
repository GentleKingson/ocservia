package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type connectionOwnerStore struct{ database.Tx }

func (t *transaction) ConnectionOwnerStore() ownerstore.Store { return connectionOwnerStore{t} }

func (s connectionOwnerStore) Acquire(ctx context.Context, t ownerstore.Term, ttl time.Duration) (v ownerstore.Lease, err error) {
	// Serialize absent-row acquisition as well as existing terms. The row lock
	// also waits for observer guards before taking a fresh authority clock.
	if _, err = s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "connection-owner:"+hex.EncodeToString(t.NodeID[:])); err != nil {
		return v, err
	}
	var previousOwner uuid.UUID
	var previousIncarnation, epoch int64
	var previousUntil value.Timestamp
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,owner_epoch,lease_until FROM connection_owner_fencing WHERE node_id=$1 FOR UPDATE`, t.NodeID[:]).Scan(&previousOwner, &previousIncarnation, &epoch, &previousUntil)
	absent := errors.Is(err, database.ErrNotFound)
	if err != nil && !absent {
		return v, err
	}
	at, err := database.WallTime(ctx, s.Tx)
	if err != nil {
		return v, err
	}
	v.Until, err = at.Add(ttl)
	if err != nil {
		return v, err
	}
	if absent {
		v.Epoch = 1
		_, err = s.Exec(ctx, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,updated_at) VALUES($1,$2,$3,$4,1,$5,$6)`, t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], v.Until, at)
		return v, err
	}
	if previousUntil.Micros > at.Micros && (previousOwner != t.Identity.InstanceID || previousIncarnation != t.Identity.Incarnation) {
		return v, database.ErrNotFound
	}
	if epoch == math.MaxInt64 {
		return v, database.ErrConstraint
	}
	v.Epoch = epoch + 1
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET owner_instance_id=$2,owner_incarnation=$3,connection_id=$4,owner_epoch=$5,lease_until=$6,updated_at=$7 WHERE node_id=$1 AND owner_epoch=$8`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], v.Epoch, v.Until, at, epoch)
	if err == nil && n != 1 {
		err = database.ErrNotFound
	}
	return
}

func (s connectionOwnerStore) Renew(ctx context.Context, t ownerstore.Term, ttl time.Duration) (until value.Timestamp, err error) {
	at, err := s.lockLiveTerm(ctx, t)
	if err != nil {
		return until, err
	}
	until, err = at.Add(ttl)
	if err != nil {
		return until, err
	}
	err = s.QueryRow(ctx, `UPDATE connection_owner_fencing SET lease_until=$6,updated_at=$7
		WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>$7 RETURNING lease_until`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch, until, at).Scan(&until)
	return
}

func (s connectionOwnerStore) Release(ctx context.Context, t ownerstore.Term) error {
	at, err := s.lockLiveTerm(ctx, t)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET lease_until=$6,updated_at=$6 WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>$6`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch, at)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s connectionOwnerStore) lockLiveTerm(ctx context.Context, t ownerstore.Term) (value.Timestamp, error) {
	var until value.Timestamp
	err := s.QueryRow(ctx, `SELECT lease_until FROM connection_owner_fencing WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 FOR UPDATE`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&until)
	if err != nil {
		return value.Timestamp{}, err
	}
	at, err := database.WallTime(ctx, s.Tx)
	if err == nil && until.Micros <= at.Micros {
		err = database.ErrNotFound
	}
	return at, err
}

func (s connectionOwnerStore) Assert(ctx context.Context, t ownerstore.Term) error {
	var until value.Timestamp
	err := s.QueryRow(ctx, `SELECT lease_until FROM connection_owner_fencing WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>clock_timestamp() FOR SHARE OF connection_owner_fencing`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&until)
	if err != nil {
		return err
	}
	at, err := database.WallTime(ctx, s.Tx)
	if err == nil && until.Micros <= at.Micros {
		return database.ErrNotFound
	}
	return err
}

func (s connectionOwnerStore) Read(ctx context.Context, node [16]byte) (v ownerstore.State, err error) {
	var connection []byte
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,lease_until>clock_timestamp() FROM connection_owner_fencing WHERE node_id=$1`, node[:]).
		Scan(&v.InstanceID, &v.Incarnation, &connection, &v.Epoch, &v.Until, &v.LeaseUntilValid)
	copy(v.ConnectionID[:], connection)
	return
}
