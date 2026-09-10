package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetrywrite"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type telemetryWriteStore struct{ tx database.Tx }

func (t *transaction) TelemetryWriteStore() telemetrywrite.Store { return telemetryWriteStore{t} }
func (s telemetryWriteStore) LockNode(ctx context.Context, node uuid.UUID) error {
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, node).Scan(&node)
}
func (s telemetryWriteStore) ValidateJSON(ctx context.Context, documents []json.RawMessage) error {
	encoded, err := json.Marshal(documents)
	if err != nil {
		return fmt.Errorf("%w: encode JSON documents", telemetrywrite.ErrInvalidJSON)
	}
	var documentCount int
	err = s.tx.QueryRow(ctx, `SELECT jsonb_array_length($1::jsonb)`, string(encoded)).Scan(&documentCount)
	if err != nil {
		var postgresError *pgconn.PgError
		// This statement performs only the input cast, so its data exceptions
		// describe deterministic payload incompatibility rather than a failed
		// business write. Operational database errors remain retryable.
		if errors.As(err, &postgresError) && (strings.HasPrefix(postgresError.Code, "22") || postgresError.Code == "54001") {
			return fmt.Errorf("%w: JSON document is not PostgreSQL-compatible", telemetrywrite.ErrInvalidJSON)
		}
		return fmt.Errorf("validate telemetry JSON storage compatibility: %w", err)
	}
	if documentCount != len(documents) {
		return fmt.Errorf("%w: JSON document count changed during validation", telemetrywrite.ErrInvalidJSON)
	}
	return nil
}

func (s telemetryWriteStore) InsertBatch(ctx context.Context, id, node uuid.UUID, sequence uint64, kind string, observed time.Time, size int) (bool, error) {
	n, err := s.tx.Exec(ctx, `INSERT INTO telemetry_ingest_batches (batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, id, node, sequence, kind, observed, size)
	return n > 0, err
}
func (s telemetryWriteStore) UpsertSnapshot(ctx context.Context, node uuid.UUID, snapshot telemetrywrite.Snapshot) (bool, error) {
	updated, err := s.tx.Exec(ctx, `INSERT INTO node_observed_snapshots
		(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at,dropped_security,dropped_health,dropped_aggregate,dropped_raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$15,$8,$9,$10,$2,$11,$12,$13,$14)
		ON CONFLICT (node_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,received_at=now(),boot_id=EXCLUDED.boot_id,
		agent_instance_id=EXCLUDED.agent_instance_id,agent_version=EXCLUDED.agent_version,ocserv_version=EXCLUDED.ocserv_version,
		os_release=EXCLUDED.os_release,architecture=EXCLUDED.architecture,ocserv=EXCLUDED.ocserv,system=EXCLUDED.system,path=EXCLUDED.path,
		last_heartbeat_at=GREATEST(node_observed_snapshots.last_heartbeat_at,EXCLUDED.last_heartbeat_at),
		dropped_security=GREATEST(node_observed_snapshots.dropped_security,EXCLUDED.dropped_security),
		dropped_health=GREATEST(node_observed_snapshots.dropped_health,EXCLUDED.dropped_health),
		dropped_aggregate=GREATEST(node_observed_snapshots.dropped_aggregate,EXCLUDED.dropped_aggregate),
		dropped_raw=GREATEST(node_observed_snapshots.dropped_raw,EXCLUDED.dropped_raw)
		WHERE EXCLUDED.observed_at > node_observed_snapshots.observed_at`,
		node, snapshot.ObservedAt, snapshot.BootID, snapshot.AgentInstance,
		snapshot.AgentVersion, snapshot.OcservVersion, snapshot.OSRelease,
		snapshot.Ocserv, snapshot.System, snapshot.Path,
		snapshot.Security, snapshot.Health, snapshot.Aggregate, snapshot.Raw,
		snapshot.Architecture)
	if err != nil {
		return false, err
	}

	return updated > 0, nil
}
func (s telemetryWriteStore) ReplaceSessions(ctx context.Context, node uuid.UUID, observed time.Time, sessions []telemetrywrite.Session) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_sessions WHERE node_id=$1`, node); err != nil {
		return err
	}
	for _, session := range sessions {
		if _, err := s.tx.Exec(ctx, `INSERT INTO node_sessions (node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, node, session.ID, session.Username, session.ClientIP, session.ConnectedAt, session.BytesIn, session.BytesOut, observed); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) ReplaceIPBans(ctx context.Context, node uuid.UUID, observed time.Time, bans []telemetrywrite.IPBan) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_ip_bans WHERE node_id=$1`, node); err != nil {
		return err
	}
	for _, ban := range bans {
		if _, err := s.tx.Exec(ctx, `INSERT INTO node_ip_bans (node_id,ip,seconds_remaining,observed_at) VALUES ($1,$2,$3,$4)`, node, ban.IP, ban.SecondsRemaining, observed); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) ReplaceUsers(ctx context.Context, node uuid.UUID, observed time.Time, users []telemetrywrite.User) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM observed_users WHERE node_id=$1`, node); err != nil {
		return err
	}
	for _, user := range users {
		if _, err := s.tx.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, node, user.Username, user.Enabled, user.Revision, user.Fingerprint, observed); err != nil {
			return err
		}
	}
	return nil
}
func (s telemetryWriteStore) Activate(ctx context.Context, node uuid.UUID, observed time.Time) error {
	if _, err := s.tx.Exec(ctx, `UPDATE nodes SET status='active',updated_at=GREATEST(updated_at,$2),version=version+1 WHERE id=$1 AND status='offline'`, node, observed); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, node.String())
	return err
}
func (s telemetryWriteStore) InsertUpgrade(ctx context.Context, node uuid.UUID, r telemetrywrite.Upgrade) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof)
 SELECT $1,$2,$3,$4,$5,$6,now(),$7
 WHERE EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=$1 AND u.node_id=$2 AND u.target_version=$4 AND u.package_sha256=$8 AND u.state IN ('accepted','running','unknown') AND u.completed_at IS NULL)
 ON CONFLICT (operation_id) DO NOTHING`, r.OperationID, node, r.State, r.TargetVersion, r.Detail, r.CompletedAt, r.Proof, r.PackageSHA256)
	return err
}
