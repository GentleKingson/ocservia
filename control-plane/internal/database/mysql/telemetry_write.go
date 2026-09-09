package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetrywrite"
	"github.com/google/uuid"
)

type telemetryWriteStore struct{ tx database.Tx }

func (t *transaction) TelemetryWriteStore() telemetrywrite.Store { return telemetryWriteStore{t} }
func (s telemetryWriteStore) LockNode(ctx context.Context, node uuid.UUID) error {
	var id []byte
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=? FOR UPDATE`, UUIDBytes(node)).Scan(&id)
}
func (s telemetryWriteStore) ValidateJSON(_ context.Context, documents []json.RawMessage) error {
	for _, doc := range documents {
		if _, err := value.ParseJSONB(doc); err != nil {
			return errors.Join(telemetrywrite.ErrInvalidJSON, err)
		}
	}
	return nil
}
func (s telemetryWriteStore) InsertBatch(ctx context.Context, id, node uuid.UUID, sequence uint64, kind string, observed time.Time, size int) (bool, error) {
	at, err := value.FromTime(observed)
	if err != nil {
		return false, err
	}
	received, err := database.TransactionTime(ctx, s.tx)
	if err != nil {
		return false, err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO telemetry_ingest_batches(batch_id,node_id,sequence,kind,observed_at,payload_bytes,received_at) VALUES(?,?,?,?,?,?,?)`, UUIDBytes(id), UUIDBytes(node), sequence, kind, at, size, received)
	// Duplicate suppression is restricted to the batch primary key; other
	// constraint failures remain visible and roll back the outer transaction.
	if errors.Is(err, database.ErrUnique) {
		var stored []byte
		check := s.tx.QueryRow(ctx, `SELECT batch_id FROM telemetry_ingest_batches WHERE batch_id=?`, UUIDBytes(id)).Scan(&stored)
		if check == nil {
			return false, nil
		}
	}
	return err == nil, err
}
func (s telemetryWriteStore) UpsertSnapshot(ctx context.Context, node uuid.UUID, v telemetrywrite.Snapshot) (bool, error) {
	at, err := value.FromTime(v.ObservedAt)
	if err != nil {
		return false, err
	}
	received, err := database.TransactionTime(ctx, s.tx)
	if err != nil {
		return false, err
	}
	var previous value.Timestamp
	err = s.tx.QueryRow(ctx, `SELECT observed_at FROM node_observed_snapshots WHERE node_id=? FOR UPDATE`, UUIDBytes(node)).Scan(&previous)
	exists := err == nil
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return false, err
	}
	if exists && previous.Micros >= at.Micros {
		return false, nil
	}
	docs := make([]value.JSONB, 3)
	for i, doc := range []json.RawMessage{v.Ocserv, v.System, v.Path} {
		docs[i], err = value.ParseJSONB(doc)
		if err != nil {
			return false, err
		}
	}
	if !exists {
		_, err = s.tx.Exec(ctx, "INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,`system`,path,last_heartbeat_at,dropped_security,dropped_health,dropped_aggregate,dropped_raw) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", UUIDBytes(node), at, received, v.BootID, UUIDBytes(v.AgentInstance), v.AgentVersion, v.OcservVersion, v.OSRelease, v.Architecture, docs[0], docs[1], docs[2], at, v.Security, v.Health, v.Aggregate, v.Raw)
	} else {
		_, err = s.tx.Exec(ctx, "UPDATE node_observed_snapshots SET observed_at=?,received_at=?,boot_id=?,agent_instance_id=?,agent_version=?,ocserv_version=?,os_release=?,architecture=?,ocserv=?,`system`=?,path=?,last_heartbeat_at=GREATEST(last_heartbeat_at,?),dropped_security=GREATEST(dropped_security,?),dropped_health=GREATEST(dropped_health,?),dropped_aggregate=GREATEST(dropped_aggregate,?),dropped_raw=GREATEST(dropped_raw,?) WHERE node_id=?", at, received, v.BootID, UUIDBytes(v.AgentInstance), v.AgentVersion, v.OcservVersion, v.OSRelease, v.Architecture, docs[0], docs[1], docs[2], at, v.Security, v.Health, v.Aggregate, v.Raw, UUIDBytes(node))
	}
	return err == nil, err
}
func telemetryIP(raw string) ([]byte, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return nil, err
	}
	// PostgreSQL inet retains the IPv4-mapped IPv6 family rather than unmapping it.
	return InetBytes(netip.PrefixFrom(a, a.BitLen()))
}
func (s telemetryWriteStore) ReplaceSessions(ctx context.Context, node uuid.UUID, observed time.Time, sessions []telemetrywrite.Session) error {
	at, err := value.FromTime(observed)
	if err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_sessions WHERE node_id=?`, UUIDBytes(node)); err != nil {
		return err
	}
	for _, v := range sessions {
		connected, err := value.FromTime(v.ConnectedAt)
		if err != nil {
			return err
		}
		ip, err := telemetryIP(v.ClientIP)
		if err != nil {
			return err
		}
		if _, err := s.tx.Exec(ctx, `INSERT INTO node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES(?,?,?,?,?,?,?,?)`, UUIDBytes(node), v.ID, v.Username, ip, connected, v.BytesIn, v.BytesOut, at); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) ReplaceIPBans(ctx context.Context, node uuid.UUID, observed time.Time, bans []telemetrywrite.IPBan) error {
	at, err := value.FromTime(observed)
	if err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_ip_bans WHERE node_id=?`, UUIDBytes(node)); err != nil {
		return err
	}
	for _, v := range bans {
		ip, err := telemetryIP(v.IP)
		if err != nil {
			return err
		}
		if _, err := s.tx.Exec(ctx, `INSERT INTO node_ip_bans(node_id,ip,seconds_remaining,observed_at) VALUES(?,?,?,?)`, UUIDBytes(node), ip, v.SecondsRemaining, at); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) ReplaceUsers(ctx context.Context, node uuid.UUID, observed time.Time, users []telemetrywrite.User) error {
	at, err := value.FromTime(observed)
	if err != nil {
		return err
	}
	if _, err := s.tx.Exec(ctx, `DELETE FROM observed_users WHERE node_id=?`, UUIDBytes(node)); err != nil {
		return err
	}
	for _, v := range users {
		if _, err := s.tx.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at) VALUES(?,?,?,?,?,?)`, UUIDBytes(node), v.Username, v.Enabled, v.Revision, v.Fingerprint, at); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) Activate(ctx context.Context, node uuid.UUID, observed time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE nodes SET status='active',updated_at=GREATEST(updated_at,?),version=version+1 WHERE id=? AND status='offline'`, observed, UUIDBytes(node))
	// The PostgreSQL NOTIFY wakeup is not a durable write. MySQL transport
	// polling is a separate, still-unported Controller integration boundary.
	return err
}
func (s telemetryWriteStore) AttestationKey(ctx context.Context, node uuid.UUID, id string) (telemetrywrite.Key, error) {
	var k telemetrywrite.Key
	err := s.tx.QueryRow(ctx, `SELECT public_key,state,activated_at,valid_until FROM node_privd_attestation_keys WHERE node_id=? AND CAST(key_id AS BINARY)=CAST(? AS BINARY)`, UUIDBytes(node), id).Scan(&k.PublicKey, &k.State, &k.ActivatedAt, &k.ValidUntil)
	return k, err
}
func (s telemetryWriteStore) InsertUpgrade(ctx context.Context, node uuid.UUID, r telemetrywrite.Upgrade) error {
	at, err := value.FromTime(r.CompletedAt)
	if err != nil {
		return err
	}
	reported, err := database.TransactionTime(ctx, s.tx)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof) SELECT ?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=? AND u.node_id=? AND CAST(u.target_version AS BINARY)=CAST(? AS BINARY) AND u.package_sha256=? AND u.state IN ('accepted','running','unknown') AND u.completed_at IS NULL)`, UUIDBytes(r.OperationID), UUIDBytes(node), r.State, r.TargetVersion, r.Detail, at, reported, r.Proof, UUIDBytes(r.OperationID), UUIDBytes(node), r.TargetVersion, r.PackageSHA256)
	if errors.Is(err, database.ErrUnique) {
		var id []byte
		if check := s.tx.QueryRow(ctx, `SELECT operation_id FROM node_agent_upgrade_results WHERE operation_id=?`, UUIDBytes(r.OperationID)).Scan(&id); check == nil {
			return nil
		}
	}
	return err
}
