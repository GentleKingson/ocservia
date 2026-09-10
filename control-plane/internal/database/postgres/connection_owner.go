package postgres

import (
	"context"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/connectionowner/ownerstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
)

type connectionOwnerStore struct{ database.Tx }

func (t *transaction) ConnectionOwnerStore() ownerstore.Store { return connectionOwnerStore{t} }

func (s connectionOwnerStore) Acquire(ctx context.Context, t ownerstore.Term, ttl time.Duration) (v ownerstore.Lease, err error) {
	err = s.QueryRow(ctx, `INSERT INTO connection_owner_fencing(node_id,owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until,updated_at)
		VALUES($1,$2,$3,$4,1,now()+$5::interval,now()) ON CONFLICT(node_id) DO UPDATE SET
		owner_instance_id=EXCLUDED.owner_instance_id,owner_incarnation=EXCLUDED.owner_incarnation,connection_id=EXCLUDED.connection_id,
		owner_epoch=connection_owner_fencing.owner_epoch+1,lease_until=EXCLUDED.lease_until,updated_at=now()
		WHERE connection_owner_fencing.lease_until<=now() OR (connection_owner_fencing.owner_instance_id=EXCLUDED.owner_instance_id AND connection_owner_fencing.owner_incarnation=EXCLUDED.owner_incarnation)
		RETURNING owner_epoch,lease_until`, t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], ttl.String()).Scan(&v.Epoch, &v.Until)
	return
}

func (s connectionOwnerStore) Renew(ctx context.Context, t ownerstore.Term, ttl time.Duration) (until value.Timestamp, err error) {
	err = s.QueryRow(ctx, `UPDATE connection_owner_fencing SET lease_until=now()+$6::interval,updated_at=now()
		WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>now() RETURNING lease_until`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch, ttl.String()).Scan(&until)
	return
}

func (s connectionOwnerStore) Release(ctx context.Context, t ownerstore.Term) error {
	n, err := s.Exec(ctx, `UPDATE connection_owner_fencing SET lease_until=now(),updated_at=now() WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>now()`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch)
	if err == nil && n != 1 {
		return database.ErrNotFound
	}
	return err
}

func (s connectionOwnerStore) Assert(ctx context.Context, t ownerstore.Term) error {
	var one int
	return s.QueryRow(ctx, `SELECT 1 FROM connection_owner_fencing WHERE node_id=$1 AND owner_instance_id=$2 AND owner_incarnation=$3 AND connection_id=$4 AND owner_epoch=$5 AND lease_until>clock_timestamp() FOR SHARE OF connection_owner_fencing`,
		t.NodeID[:], t.Identity.InstanceID, t.Identity.Incarnation, t.ConnectionID[:], t.Epoch).Scan(&one)
}

func (s connectionOwnerStore) Read(ctx context.Context, node [16]byte) (v ownerstore.State, err error) {
	var connection []byte
	err = s.QueryRow(ctx, `SELECT owner_instance_id,owner_incarnation,connection_id,owner_epoch,lease_until>clock_timestamp() FROM connection_owner_fencing WHERE node_id=$1`, node[:]).
		Scan(&v.InstanceID, &v.Incarnation, &connection, &v.Epoch, &v.LeaseUntilValid)
	copy(v.ConnectionID[:], connection)
	return
}
