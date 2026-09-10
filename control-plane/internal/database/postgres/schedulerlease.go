package postgres

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/schedulerlease"
)

type SchedulerLeaseStore struct{ tx database.Tx }

func (s *SchedulerLeaseStore) RecordMaintenanceCompletion(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	_, err := s.tx.Exec(ctx, `SELECT public.g6_record_scheduler_maintenance($1,$2,$3)`, owner.InstanceID, owner.Incarnation, epoch)
	return err
}

func (t *transaction) SchedulerLeaseStore() schedulerlease.Store { return &SchedulerLeaseStore{tx: t} }
func (s *SchedulerLeaseStore) Lock(ctx context.Context) (schedulerlease.State, error) {
	var v schedulerlease.State
	err := s.tx.QueryRow(ctx, `SELECT instance_id,incarnation,epoch,lease_until FROM scheduler_leadership WHERE id=1 FOR UPDATE`).Scan(&v.Owner.InstanceID, &v.Owner.Incarnation, &v.Epoch, &v.Until)
	return v, err
}
func (s *SchedulerLeaseStore) Put(ctx context.Context, v schedulerlease.State, now value.Timestamp) error {
	n, err := s.tx.Exec(ctx, `UPDATE scheduler_leadership SET instance_id=$1,incarnation=$2,epoch=$3,lease_until=$4,updated_at=$5 WHERE id=1`, v.Owner.InstanceID, v.Owner.Incarnation, v.Epoch, v.Until, now)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}
func (s *SchedulerLeaseStore) Assert(ctx context.Context, owner schedulerlease.Owner, epoch int64) error {
	var until value.Timestamp
	err := s.tx.QueryRow(ctx, `SELECT lease_until FROM scheduler_leadership WHERE id=1 AND instance_id=$1 AND incarnation=$2 AND epoch=$3 AND lease_until>clock_timestamp() FOR SHARE OF scheduler_leadership`, owner.InstanceID, owner.Incarnation, epoch).Scan(&until)
	if errors.Is(err, database.ErrNotFound) {
		return schedulerlease.ErrLost
	}
	if err != nil {
		return err
	}
	at, err := database.WallTime(ctx, s.tx)
	if err == nil && until.Micros <= at.Micros {
		return schedulerlease.ErrLost
	}
	return err
}
