package mysql

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
	"github.com/google/uuid"
)

type SchedulerLeaseStore struct{ tx database.Tx }

func (s *SchedulerLeaseStore) RecordMaintenanceCompletion(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	_, err := s.tx.Exec(ctx, `CALL g6_record_scheduler_maintenance(?,?,?)`, UUIDBytes(owner.InstanceID), owner.Incarnation, epoch)
	return err
}

func (t *transaction) SchedulerLeaseStore() schedulerlease.Store { return &SchedulerLeaseStore{tx: t} }
func (s *SchedulerLeaseStore) Lock(ctx context.Context) (schedulerlease.State, error) {
	var v schedulerlease.State
	var id []byte
	err := s.tx.QueryRow(ctx, `SELECT instance_id,incarnation,epoch,lease_until FROM scheduler_leadership WHERE id=1 FOR UPDATE`).Scan(&id, &v.Owner.Incarnation, &v.Epoch, &v.Until)
	if err != nil {
		return v, err
	}
	v.Owner.InstanceID, err = uuid.FromBytes(id)
	return v, err
}
func (s *SchedulerLeaseStore) Put(ctx context.Context, v schedulerlease.State, now value.Timestamp) error {
	_, err := s.tx.Exec(ctx, `UPDATE scheduler_leadership SET instance_id=?,incarnation=?,epoch=?,lease_until=?,updated_at=? WHERE id=1`, UUIDBytes(v.Owner.InstanceID), v.Owner.Incarnation, v.Epoch, v.Until, now)
	return err
}
func (s *SchedulerLeaseStore) Assert(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	var until value.Timestamp
	err := s.tx.QueryRow(ctx, `SELECT lease_until FROM scheduler_leadership WHERE id=1 AND instance_id=? AND incarnation=? AND epoch=? AND lease_until>TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6)) LOCK IN SHARE MODE`, UUIDBytes(owner.InstanceID), owner.Incarnation, epoch).Scan(&until)
	if errors.Is(err, database.ErrNotFound) {
		return schedulerlease.ErrLost
	}
	if err != nil {
		return err
	}
	// UTC_TIMESTAMP is statement-stable. Check again after obtaining the lock,
	// so a lock wait cannot turn an expired lease into a successful fence.
	var wall value.Timestamp
	if err = s.tx.QueryRow(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND,'2000-01-01 00:00:00',UTC_TIMESTAMP(6))`).Scan(&wall); err != nil {
		return err
	}
	if until.Micros <= wall.Micros {
		return schedulerlease.ErrLost
	}
	return nil
}
