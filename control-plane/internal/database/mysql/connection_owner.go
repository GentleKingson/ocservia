package mysql

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
	// A transaction-owned node key also serializes the first acquisition,
	// when there is not yet an authority row to lock.
	if err = LockTransaction(ctx, s.Tx, "connection-owner:"+hex.EncodeToString(t.NodeID[:])); err != nil {
		return v, err
	}
	var previousOwner uuid.UUID
	var previousIncarnation, epoch int64
	var previousUntil value.Timestamp
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,owner_epoch,lease_until FROM connection_owner_fencing WHERE node_id=? FOR UPDATE`, t.NodeID[:]).Scan(&previousOwner, &previousIncarnation, &epoch, &previousUntil)
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
		_, err = s.Exec(ctx, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,updated_at)VALUES(?,?,?,?,1,?,?)`, t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], v.Until, at)
		return v, err
	}
	if previousUntil.Micros > at.Micros && (previousOwner != t.Identity.InstanceID || previousIncarnation != t.Identity.Incarnation) {
		return v, database.ErrNotFound
	}
	if epoch == math.MaxInt64 {
		return v, database.ErrConstraint
	}
	v.Epoch = epoch + 1
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET owner_instance_id=?,owner_incarnation=?,connection_id=?,owner_epoch=?,lease_until=?,updated_at=? WHERE node_id=? AND owner_epoch=?`,
		UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], v.Epoch, v.Until, at, t.NodeID[:], epoch)
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
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET lease_until=?,updated_at=? WHERE node_id=? AND owner_instance_id=? AND owner_incarnation=? AND connection_id=? AND owner_epoch=? AND lease_until>?`,
		until, at, t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], t.Epoch, at)
	if err == nil && n == 0 {
		stored, readErr := s.Read(ctx, t.NodeID)
		if readErr != nil {
			return until, readErr
		}
		if stored.InstanceID != t.Identity.InstanceID || stored.Incarnation != t.Identity.Incarnation || stored.ConnectionID != t.ConnectionID || stored.Epoch != t.Epoch || stored.Until != until {
			return until, database.ErrNotFound
		}
	}
	return
}

func (s connectionOwnerStore) Release(ctx context.Context, t ownerstore.Term) error {
	at, err := s.lockLiveTerm(ctx, t)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET lease_until=?,updated_at=? WHERE node_id=? AND owner_instance_id=? AND owner_incarnation=? AND connection_id=? AND owner_epoch=? AND lease_until>?`,
		at, at, t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], t.Epoch, at)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s connectionOwnerStore) lockLiveTerm(ctx context.Context, t ownerstore.Term) (value.Timestamp, error) {
	var until value.Timestamp
	err := s.QueryRow(ctx, `SELECT lease_until FROM connection_owner_fencing WHERE node_id=? AND owner_instance_id=? AND owner_incarnation=? AND connection_id=? AND owner_epoch=? FOR UPDATE`,
		t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&until)
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
	// InnoDB can retain a record lock even for a filtered-out row. Reject an
	// already-expired term before locking, then recheck after any lock wait.
	var live bool
	if err := s.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM connection_owner_fencing WHERE node_id=? AND owner_instance_id=? AND owner_incarnation=? AND connection_id=? AND owner_epoch=? AND lease_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)))`,
		t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&live); err != nil {
		return err
	}
	if !live {
		return database.ErrNotFound
	}
	var until, now value.Timestamp
	err := s.QueryRow(ctx, `SELECT lease_until FROM connection_owner_fencing WHERE node_id=? AND owner_instance_id=? AND owner_incarnation=? AND connection_id=? AND owner_epoch=?
		AND lease_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)) LOCK IN SHARE MODE`, t.NodeID[:], UUIDBytes(t.Identity.InstanceID), t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&until)
	if err != nil {
		return err
	}
	if err := s.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6))`).Scan(&now); err != nil {
		return err
	}
	if until.Micros <= now.Micros {
		return database.ErrNotFound
	}
	return nil
}

func (s connectionOwnerStore) Read(ctx context.Context, node [16]byte) (v ownerstore.State, err error) {
	var connection []byte
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,lease_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',CURRENT_TIMESTAMP(6)) FROM connection_owner_fencing WHERE node_id=?`, node[:]).
		Scan(&v.InstanceID, &v.Incarnation, &connection, &v.Epoch, &v.Until, &v.LeaseUntilValid)
	copy(v.ConnectionID[:], connection)
	return
}
