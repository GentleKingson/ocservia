package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type telemetryStore struct{ tx database.Tx }

func (t *transaction) TelemetryStore() telemetrystore.Store { return telemetryStore{t} }

const telemetryNodeSelect = `SELECT n.id,n.name,n.version,n.status,o.observed_at,o.last_heartbeat_at,COALESCE(o.boot_id,''),COALESCE(o.agent_instance_id::text,''),COALESCE(o.agent_version,''),COALESCE(o.ocserv_version,''),COALESCE(o.os_release,''),COALESCE(o.architecture,''),o.ocserv,o.system,o.path,COALESCE(o.dropped_security,0),COALESCE(o.dropped_health,0),COALESCE(o.dropped_aggregate,0),COALESCE(o.dropped_raw,0),(SELECT count(*) FROM node_sessions ss WHERE ss.node_id=n.id),COALESCE(c.revision,0),(EXISTS(SELECT 1 FROM node_capabilities nc WHERE nc.node_id=n.id AND nc.capability IN ('ocserv.agent.upgrade.v1','ocserv.agent.upgrade.v2') AND nc.approved=true) AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations auo WHERE auo.node_id=n.id AND auo.completed_at IS NULL AND auo.state IN ('queued','accepted','running','unknown'))) FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id LEFT JOIN node_config_state c ON c.node_id=n.id`

func scanTelemetryNode(row database.Row) (telemetrystore.Node, error) {
	var n telemetrystore.Node
	err := row.Scan(&n.ID, &n.Name, &n.Version, &n.Status, &n.ObservedAt, &n.Heartbeat, &n.BootID, &n.InstanceID, &n.AgentVersion, &n.OcservVersion, &n.OSRelease, &n.Architecture, &n.Ocserv, &n.System, &n.Path, &n.Security, &n.Health, &n.Aggregate, &n.Raw, &n.Sessions, &n.ConfigRevision, &n.UpgradeEligible)
	return n, err
}

func (s telemetryStore) Nodes(ctx context.Context, workspace, after uuid.UUID, limit int) ([]telemetrystore.Node, error) {
	var scope, cursor any
	if workspace != uuid.Nil {
		scope = workspace
	}
	if after != uuid.Nil {
		cursor = after
	}
	rows, err := s.tx.Query(ctx, telemetryNodeSelect+` WHERE ($1::uuid IS NULL OR n.id>$1) AND ($2::uuid IS NULL OR n.workspace_id=$2) ORDER BY n.id LIMIT $3`, cursor, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []telemetrystore.Node{}
	for rows.Next() {
		n, err := scanTelemetryNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s telemetryStore) Node(ctx context.Context, id uuid.UUID) (telemetrystore.Node, error) {
	return scanTelemetryNode(s.tx.QueryRow(ctx, telemetryNodeSelect+` WHERE n.id=$1`, id))
}

func (s telemetryStore) Sessions(ctx context.Context, id uuid.UUID, after string, limit int) ([]telemetrystore.ReadSession, error) {
	rows, err := s.tx.Query(ctx, `SELECT session_id,username,host(client_ip),connected_at,bytes_in,bytes_out FROM node_sessions WHERE node_id=$1 AND session_id>$2 ORDER BY session_id LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []telemetrystore.ReadSession{}
	for rows.Next() {
		var v telemetrystore.ReadSession
		if err := rows.Scan(&v.ID, &v.Username, &v.ClientIP, &v.ConnectedAt, &v.BytesIn, &v.BytesOut); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s telemetryStore) IPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetrystore.IPBan, error) {
	rows, err := s.tx.Query(ctx, `SELECT host(ip),seconds_remaining FROM node_ip_bans WHERE node_id=$1 ORDER BY ip LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []telemetrystore.IPBan{}
	for rows.Next() {
		var v telemetrystore.IPBan
		if err := rows.Scan(&v.IP, &v.SecondsRemaining); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s telemetryStore) LockNode(ctx context.Context, node uuid.UUID) error {
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, node).Scan(&node)
}

func (s telemetryStore) ValidateJSON(ctx context.Context, documents []json.RawMessage) error {
	encoded, err := json.Marshal(documents)
	if err != nil {
		return fmt.Errorf("%w: encode JSON documents", telemetrystore.ErrInvalidJSON)
	}
	var documentCount int
	err = s.tx.QueryRow(ctx, `SELECT jsonb_array_length($1::jsonb)`, string(encoded)).Scan(&documentCount)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && (strings.HasPrefix(postgresError.Code, "22") || postgresError.Code == "54001") {
			return fmt.Errorf("%w: JSON document is not PostgreSQL-compatible", telemetrystore.ErrInvalidJSON)
		}
		return fmt.Errorf("validate telemetry JSON storage compatibility: %w", err)
	}
	if documentCount != len(documents) {
		return fmt.Errorf("%w: JSON document count changed during validation", telemetrystore.ErrInvalidJSON)
	}
	return nil
}

func (s telemetryStore) InsertBatch(ctx context.Context, id, node uuid.UUID, sequence uint64, kind string, observed time.Time, size int) (bool, error) {
	n, err := s.tx.Exec(ctx, `INSERT INTO telemetry_ingest_batches (batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, id, node, sequence, kind, observed, size)
	return n > 0, err
}

func (s telemetryStore) UpsertSnapshot(ctx context.Context, node uuid.UUID, snapshot telemetrystore.Snapshot) (bool, error) {
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

func (s telemetryStore) ReplaceSessions(ctx context.Context, node uuid.UUID, observed time.Time, sessions []telemetrystore.Session) error {
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

func (s telemetryStore) ReplaceIPBans(ctx context.Context, node uuid.UUID, observed time.Time, bans []telemetrystore.IPBan) error {
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

func (s telemetryStore) ReplaceUsers(ctx context.Context, node uuid.UUID, observed time.Time, users []telemetrystore.User) error {
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

func (s telemetryStore) Activate(ctx context.Context, node uuid.UUID, observed time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE nodes SET status='active',updated_at=GREATEST(updated_at,$2),version=version+1 WHERE id=$1 AND status='offline'`, node, observed)
	return err
}

func (s telemetryStore) LockOfflineCandidates(ctx context.Context, before time.Time) ([]uuid.UUID, error) {
	return database.CollectColumn[uuid.UUID](s.tx.Query(ctx, `SELECT n.id FROM nodes n JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.status='active' AND o.last_heartbeat_at < $1 FOR UPDATE OF n SKIP LOCKED`, before))
}

func (s telemetryStore) MarkOffline(ctx context.Context, node, event uuid.UUID, now time.Time, traceparent string) error {
	if _, err := s.tx.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, node, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload) VALUES($1,$2,'disconnected',$3,$4,$5)`, event, node, now, traceparent, []byte("heartbeat timeout"))
	return err
}

func (s telemetryStore) InsertUpgrade(ctx context.Context, node uuid.UUID, r telemetrystore.Upgrade) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO node_agent_upgrade_results(operation_id,node_id,state,target_version,detail,completed_at,reported_at,privileged_result_proof)
 SELECT $1,$2,$3,$4,$5,$6,now(),$7
 WHERE EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=$1 AND u.node_id=$2 AND u.target_version=$4 AND u.package_sha256=$8 AND u.state IN ('accepted','running','unknown') AND u.completed_at IS NULL)
 ON CONFLICT (operation_id) DO NOTHING`, r.OperationID, node, r.State, r.TargetVersion, r.Detail, r.CompletedAt, r.Proof, r.PackageSHA256)
	return err
}

func (s telemetryStore) ReplaceGroups(ctx context.Context, node uuid.UUID, groups []telemetrystore.Group) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM observed_groups WHERE node_id=$1`, node); err != nil {
		return err
	}
	for _, g := range groups {
		if _, err := s.tx.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, node, g.Name, g.Members, g.Revision, g.Fingerprint, g.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s telemetryStore) Groups(ctx context.Context, node uuid.UUID) ([]telemetrystore.Group, error) {
	rows, err := s.tx.Query(ctx, `SELECT group_name,members,revision,fingerprint,observed_at FROM observed_groups WHERE node_id=$1 ORDER BY group_name COLLATE "C"`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []telemetrystore.Group{}
	for rows.Next() {
		var g telemetrystore.Group
		if err := rows.Scan(&g.Name, &g.Members, &g.Revision, &g.Fingerprint, &g.ObservedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s telemetryStore) InsertSecurity(ctx context.Context, node uuid.UUID, e telemetrystore.SecurityEvent) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, e.ID, node, e.ObservedAt, e.Severity, e.Type, e.Detail)
	return err
}

func (s telemetryStore) SecurityEvent(ctx context.Context, id uuid.UUID) (telemetrystore.SecurityEvent, error) {
	e := telemetrystore.SecurityEvent{ID: id}
	err := s.tx.QueryRow(ctx, `SELECT observed_at,severity,event_type,detail FROM telemetry_security_events WHERE event_id=$1`, id).Scan(&e.ObservedAt, &e.Severity, &e.Type, &e.Detail)
	return e, err
}
