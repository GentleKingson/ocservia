package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
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
	var (
		rows database.Rows
		err  error
	)
	switch {
	case workspace == uuid.Nil && after == uuid.Nil:
		rows, err = s.tx.Query(ctx, telemetryNodeSelect+` ORDER BY n.id LIMIT $1`, limit)
	case workspace == uuid.Nil:
		rows, err = s.tx.Query(ctx, telemetryNodeSelect+` WHERE n.id>$1 ORDER BY n.id LIMIT $2`, after, limit)
	case after == uuid.Nil:
		rows, err = s.tx.Query(ctx, telemetryNodeSelect+` WHERE n.workspace_id=$1 ORDER BY n.id LIMIT $2`, workspace, limit)
	default:
		rows, err = s.tx.Query(ctx, telemetryNodeSelect+` WHERE n.workspace_id=$1 AND n.id>$2 ORDER BY n.id LIMIT $3`, workspace, after, limit)
	}
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
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR NO KEY UPDATE`, node).Scan(&node)
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
	if len(sessions) == 0 {
		return nil
	}
	ids := make([]string, len(sessions))
	usernames := make([]string, len(sessions))
	ips := make([]string, len(sessions))
	connectedAt := make([]time.Time, len(sessions))
	bytesIn := make([]int64, len(sessions))
	bytesOut := make([]int64, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
		usernames[i] = session.Username
		ips[i] = session.ClientIP
		connectedAt[i] = session.ConnectedAt
		bytesIn[i] = session.BytesIn
		bytesOut[i] = session.BytesOut
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO node_sessions (node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at)
		SELECT $1,u.session_id,u.username,u.client_ip::inet,u.connected_at,u.bytes_in,u.bytes_out,$8
		FROM unnest($2::text[],$3::text[],$4::text[],$5::timestamptz[],$6::bigint[],$7::bigint[]) AS u(session_id,username,client_ip,connected_at,bytes_in,bytes_out)`,
		node, ids, usernames, ips, connectedAt, bytesIn, bytesOut, observed)
	return err
}

func (s telemetryStore) ReplaceIPBans(ctx context.Context, node uuid.UUID, observed time.Time, bans []telemetrystore.IPBan) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_ip_bans WHERE node_id=$1`, node); err != nil {
		return err
	}
	if len(bans) == 0 {
		return nil
	}
	ips := make([]string, len(bans))
	seconds := make([]*int64, len(bans))
	for i, ban := range bans {
		ips[i] = ban.IP
		if ban.SecondsRemaining != nil {
			if *ban.SecondsRemaining > math.MaxInt64 {
				return database.ErrConstraint
			}
			v := int64(*ban.SecondsRemaining)
			seconds[i] = &v
		}
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO node_ip_bans (node_id,ip,seconds_remaining,observed_at)
		SELECT $1,u.ip::inet,u.seconds_remaining,$4
		FROM unnest($2::text[],$3::bigint[]) AS u(ip,seconds_remaining)`,
		node, ips, seconds, observed)
	return err
}

func (s telemetryStore) ReplaceUsers(ctx context.Context, node uuid.UUID, observed time.Time, users []telemetrystore.User) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM observed_users WHERE node_id=$1`, node); err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	usernames := make([]string, len(users))
	enabled := make([]bool, len(users))
	revisions := make([]int64, len(users))
	fingerprints := make([][]byte, len(users))
	for i, user := range users {
		if user.Revision > math.MaxInt64 {
			return database.ErrConstraint
		}
		usernames[i] = user.Username
		enabled[i] = user.Enabled
		revisions[i] = int64(user.Revision)
		fingerprints[i] = user.Fingerprint
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at)
		SELECT $1,u.username,u.enabled,u.revision,u.fingerprint,$6
		FROM unnest($2::text[],$3::boolean[],$4::bigint[],$5::bytea[]) AS u(username,enabled,revision,fingerprint)`,
		node, usernames, enabled, revisions, fingerprints, observed)
	return err
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
	const chunkSize = 100
	for start := 0; start < len(groups); start += chunkSize {
		chunk := groups[start:min(start+chunkSize, len(groups))]
		var query strings.Builder
		query.Grow(84 + len(chunk)*28)
		query.WriteString(`INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES `)
		args := make([]any, 1, 1+len(chunk)*5)
		args[0] = node
		for i, g := range chunk {
			if i > 0 {
				query.WriteByte(',')
			}
			base := 2 + i*5
			fmt.Fprintf(&query, "($1,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4)
			args = append(args, g.Name, g.Members, g.Revision, g.Fingerprint, g.ObservedAt)
		}
		if _, err := s.tx.Exec(ctx, query.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func (s telemetryStore) InsertSecurity(ctx context.Context, node uuid.UUID, e telemetrystore.SecurityEvent) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, e.ID, node, e.ObservedAt, e.Severity, e.Type, e.Detail)
	return err
}

func (s telemetryStore) InsertHistory(ctx context.Context, nodeID, batchID uuid.UUID, samples []telemetrystore.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	sampledAt := make([]time.Time, len(samples))
	metrics := make([]string, len(samples))
	values := make([]float64, len(samples))
	for i, sample := range samples {
		sampledAt[i] = sample.SampledAt
		metrics[i] = sample.Metric
		values[i] = sample.Value
	}
	if _, err := s.tx.Exec(ctx, `INSERT INTO telemetry_samples(node_id,batch_id,sampled_at,metric,value)
		SELECT $1,$2,u.sampled_at,u.metric,u.value
		FROM unnest($3::timestamptz[],$4::text[],$5::double precision[]) AS u(sampled_at,metric,value)
		ON CONFLICT DO NOTHING`, nodeID, batchID, sampledAt, metrics, values); err != nil {
		return fmt.Errorf("insert telemetry sample: %w", err)
	}
	return nil
}

func (s telemetryStore) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since value.Timestamp) ([]telemetrystore.Point, error) {
	if err := since.Validate(); err != nil {
		return nil, err
	}
	if !since.Valid {
		return nil, database.ErrConstraint
	}
	query := `SELECT sampled_at,metric,1,value,value,value FROM telemetry_samples WHERE node_id=$1 AND metric=$2 AND sampled_at >= $3 ORDER BY sampled_at LIMIT 2000`
	switch resolution {
	case "raw":
	case "5m", "1h":
		query = fmt.Sprintf(`SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_%s WHERE node_id=$1 AND metric=$2 AND bucket_at >= $3 ORDER BY bucket_at LIMIT 2000`, resolution)
	default:
		return nil, database.ErrConstraint
	}
	rows, err := s.tx.Query(ctx, query, nodeID, metric, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []telemetrystore.Point{}
	for rows.Next() {
		var p telemetrystore.Point
		if err := rows.Scan(&p.At, &p.Metric, &p.Count, &p.Minimum, &p.Maximum, &p.Average); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

func (s telemetryStore) MaintainHistory(ctx context.Context, now time.Time) error {
	for _, table := range []struct {
		name, interval string
		width          time.Duration
	}{{"telemetry_rollups_5m", "5 minutes", 5 * time.Minute}, {"telemetry_rollups_1h", "1 hour", time.Hour}} {
		// Recompute the entire accepted lateness window, including the complete
		// first bucket; a partial bucket would overwrite its earlier samples.
		since := now.UTC().Add(-14 * 24 * time.Hour).Truncate(table.width)
		_, err := s.tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s AS existing(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,date_bin($1::interval,sampled_at,'2000-01-01 00:00:00+00'::timestamptz),count(*),min(value),max(value),avg(value) FROM telemetry_samples WHERE sampled_at >= $2 GROUP BY 1,2,3 ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value WHERE (existing.sample_count,existing.min_value,existing.max_value,existing.avg_value) IS DISTINCT FROM (EXCLUDED.sample_count,EXCLUDED.min_value,EXCLUDED.max_value,EXCLUDED.avg_value)`, table.name), table.interval, since)
		if err != nil {
			return err
		}
	}
	if _, err := s.tx.Exec(ctx, `SELECT telemetry_prune_rollups($1)`, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `SELECT telemetry_drop_expired_partitions($1)`, now.Add(-14*24*time.Hour))
	return err
}

var _ telemetrystore.Store = telemetryStore{}

func (b *Backend) ValidateTelemetryRuntime(ctx context.Context) error {
	var safe bool
	err := b.QueryRow(ctx, `SELECT NOT has_schema_privilege(current_user,'public','CREATE') AND NOT has_table_privilege(current_user,'telemetry_rollups_5m','DELETE,TRUNCATE') AND NOT has_table_privilege(current_user,'telemetry_rollups_1h','DELETE,TRUNCATE') AND has_function_privilege(current_user,'telemetry_prune_rollups(timestamptz)','EXECUTE') AND has_function_privilege(current_user,'telemetry_drop_expired_partitions(timestamptz)','EXECUTE') AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("runtime telemetry maintenance privileges are missing or unrestricted: %w", database.ErrPermission)
	}
	if err := b.QueryRow(ctx, `SELECT NOT has_function_privilege(current_user,'telemetry_ensure_month_partition(timestamptz)','EXECUTE')`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("runtime has indirect telemetry partition DDL capability: %w", database.ErrPermission)
	}
	if err := b.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM generate_series(date_trunc('month',(now()-interval '14 days') AT TIME ZONE 'UTC'),date_trunc('month',(now()+interval '5 minutes') AT TIME ZONE 'UTC'),interval '1 month') month WHERE NOT EXISTS(SELECT 1 FROM pg_inherits WHERE inhparent='public.telemetry_samples'::regclass AND inhrelid=to_regclass('public.telemetry_samples_'||to_char(month,'YYYYMM'))))`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("required telemetry month is not provisioned: %w", database.ErrConstraint)
	}
	return nil
}

// UsageStore borrows a transaction; it cannot open or commit another one.
type UsageStore struct{ tx database.Tx }

func NewUsageStore(tx database.Tx) *UsageStore { return &UsageStore{tx: tx} }

func (t *transaction) UsageStore() userstore.UsageStore { return NewUsageStore(t) }

func (s *UsageStore) LockNode(ctx context.Context, nodeID uuid.UUID) error {
	var id uuid.UUID
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR NO KEY UPDATE`, nodeID).Scan(&id)
}

func (s *UsageStore) LockCursor(ctx context.Context, nodeID uuid.UUID, sample userstore.UsageCursor) (userstore.UsageCursor, error) {
	prior := userstore.UsageCursor{SessionID: sample.SessionID, Connected: sample.Connected}
	err := s.tx.QueryRow(ctx, `SELECT username,rx_bytes,tx_bytes,observed_at FROM user_usage_cursors WHERE node_id=$1 AND session_id=$2 AND connected_at=$3 FOR UPDATE`, nodeID, sample.SessionID, sample.Connected).Scan(&prior.Username, &prior.RXBytes, &prior.TXBytes, &prior.ObservedAt)
	return prior, err
}

func (s *UsageStore) PutCursor(ctx context.Context, nodeID uuid.UUID, sample userstore.UsageCursor) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,session_id,connected_at) DO UPDATE SET username=EXCLUDED.username,rx_bytes=EXCLUDED.rx_bytes,tx_bytes=EXCLUDED.tx_bytes,observed_at=EXCLUDED.observed_at WHERE EXCLUDED.observed_at>user_usage_cursors.observed_at`, nodeID, sample.SessionID, sample.Connected, sample.Username, sample.RXBytes, sample.TXBytes, sample.ObservedAt)
	return err
}

func (s *UsageStore) AddUsage(ctx context.Context, nodeID uuid.UUID, sample userstore.UsageCursor, period string, start value.Timestamp, rx, tx int64) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(node_id,username,period,period_start) DO UPDATE SET rx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.rx_bytes::numeric+EXCLUDED.rx_bytes::numeric)::bigint,tx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.tx_bytes::numeric+EXCLUDED.tx_bytes::numeric)::bigint,observed_at=GREATEST(observed_user_usage.observed_at,EXCLUDED.observed_at)`, nodeID, sample.Username, period, start, rx, tx, sample.ObservedAt)
	return err
}

var _ userstore.UsageStore = (*UsageStore)(nil)
