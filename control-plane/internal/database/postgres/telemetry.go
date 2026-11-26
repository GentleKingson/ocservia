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
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	userstore "github.com/GentleKingson/ocservia/control-plane/internal/useroperations/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type telemetryStore struct{ tx *transaction }

func (t *transaction) TelemetryStore() telemetrystore.Store { return telemetryStore{t} }

const telemetryNodeSelect = `SELECT n.id,n.name,n.version,n.status,f.active,o.observed_at,o.last_heartbeat_at,COALESCE(o.boot_id,''),COALESCE(o.agent_instance_id::text,''),COALESCE(o.agent_version,''),COALESCE(o.ocserv_version,''),COALESCE(o.os_release,''),COALESCE(o.architecture,''),o.ocserv,o.system,o.path,COALESCE(o.dropped_security,0),COALESCE(o.dropped_health,0),COALESCE(o.dropped_aggregate,0),COALESCE(o.dropped_raw,0),(SELECT count(*) FROM node_sessions ss WHERE ss.node_id=n.id),COALESCE(c.revision,0),(EXISTS(SELECT 1 FROM node_capabilities nc WHERE nc.node_id=n.id AND nc.capability='ocserv.agent.upgrade.v2' AND nc.approved=true) AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations auo WHERE auo.node_id=n.id AND auo.completed_at IS NULL AND auo.state IN ('queued','accepted','running','unknown'))) FROM nodes n LEFT JOIN connection_owner_fencing f ON f.node_id=uuid_send(n.id) LEFT JOIN node_observed_snapshots o ON o.node_id=n.id LEFT JOIN node_config_state c ON c.node_id=n.id`

func scanTelemetryNode(row Row) (telemetrystore.Node, error) {
	var n telemetrystore.Node
	err := row.Scan(&n.ID, &n.Name, &n.Version, &n.Status, &n.OwnerActive, &n.ObservedAt, &n.Heartbeat, &n.BootID, &n.InstanceID, &n.AgentVersion, &n.OcservVersion, &n.OSRelease, &n.Architecture, &n.Ocserv, &n.System, &n.Path, &n.Security, &n.Health, &n.Aggregate, &n.Raw, &n.Sessions, &n.ConfigRevision, &n.UpgradeEligible)
	return n, err
}

func (s telemetryStore) Nodes(ctx context.Context, workspace, after uuid.UUID, limit int) ([]telemetrystore.Node, error) {
	var suffix string
	var args []any
	switch {
	case workspace == uuid.Nil && after == uuid.Nil:
		suffix, args = ` ORDER BY n.id LIMIT $1`, []any{limit}
	case workspace == uuid.Nil:
		suffix, args = ` WHERE n.id>$1 ORDER BY n.id LIMIT $2`, []any{after, limit}
	case after == uuid.Nil:
		suffix, args = ` WHERE n.workspace_id=$1 ORDER BY n.id LIMIT $2`, []any{workspace, limit}
	default:
		suffix, args = ` WHERE n.workspace_id=$1 AND n.id>$2 ORDER BY n.id LIMIT $3`, []any{workspace, after, limit}
	}
	return collectRows(ctx, s.tx.store, telemetryNodeSelect+suffix, func(r Rows) (telemetrystore.Node, error) {
		return scanTelemetryNode(Row{r})
	}, args...)
}

func (s telemetryStore) Node(ctx context.Context, id uuid.UUID) (telemetrystore.Node, error) {
	return scanTelemetryNode(s.tx.QueryRow(ctx, telemetryNodeSelect+` WHERE n.id=$1`, id))
}

func (s telemetryStore) Sessions(ctx context.Context, id uuid.UUID, after string, limit int) ([]telemetrystore.ReadSession, error) {
	return collectRows(ctx, s.tx.store, `SELECT session_id,username,host(client_ip),connected_at,bytes_in,bytes_out FROM node_sessions WHERE node_id=$1 AND session_id>$2 ORDER BY session_id LIMIT $3`, func(r Rows) (v telemetrystore.ReadSession, err error) {
		err = r.Scan(&v.ID, &v.Username, &v.ClientIP, &v.ConnectedAt, &v.BytesIn, &v.BytesOut)
		return
	}, id, after, limit)
}

func (s telemetryStore) IPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetrystore.IPBan, error) {
	return collectRows(ctx, s.tx.store, `SELECT host(ip),seconds_remaining FROM node_ip_bans WHERE node_id=$1 ORDER BY ip LIMIT $2`, func(r Rows) (v telemetrystore.IPBan, err error) {
		err = r.Scan(&v.IP, &v.SecondsRemaining)
		return
	}, id, limit)
}

func (s telemetryStore) LockNode(ctx context.Context, node uuid.UUID) error {
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR NO KEY UPDATE`, node).Scan(&node)
}

// InsertObservation writes documents through their JSONB input types. PostgreSQL
// checks the bind parameters even for duplicate batches, before the CTE runs.
func (s telemetryStore) InsertObservation(ctx context.Context, id, node uuid.UUID, sequence uint64, kind string, size int, snapshot telemetrystore.Snapshot, events []telemetrystore.SecurityEvent) (inserted, updated bool, err error) {
	ids := make([]uuid.UUID, len(events))
	times := make([]time.Time, len(events))
	severities := make([]string, len(events))
	types := make([]string, len(events))
	details := make([]json.RawMessage, len(events))
	for i, event := range events {
		ids[i], times[i], severities[i], types[i], details[i] = event.ID, event.ObservedAt, event.Severity, event.Type, event.Detail
	}
	err = s.tx.QueryRow(ctx, `WITH batch AS (
		INSERT INTO telemetry_ingest_batches (batch_id,node_id,sequence,kind,observed_at,payload_bytes)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING RETURNING 1
	), snapshot AS (
		INSERT INTO node_observed_snapshots
		(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at,dropped_security,dropped_health,dropped_aggregate,dropped_raw)
		SELECT $2,$5,$7,$8,$9,$10,$11,$19,$12::jsonb,$13::jsonb,$14::jsonb,$5,$15,$16,$17,$18 FROM batch WHERE true
		ON CONFLICT (node_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,received_at=now(),boot_id=EXCLUDED.boot_id,
		agent_instance_id=EXCLUDED.agent_instance_id,agent_version=EXCLUDED.agent_version,ocserv_version=EXCLUDED.ocserv_version,
		os_release=EXCLUDED.os_release,architecture=EXCLUDED.architecture,ocserv=EXCLUDED.ocserv,system=EXCLUDED.system,path=EXCLUDED.path,
		last_heartbeat_at=GREATEST(node_observed_snapshots.last_heartbeat_at,EXCLUDED.last_heartbeat_at),
		dropped_security=GREATEST(node_observed_snapshots.dropped_security,EXCLUDED.dropped_security),
		dropped_health=GREATEST(node_observed_snapshots.dropped_health,EXCLUDED.dropped_health),
		dropped_aggregate=GREATEST(node_observed_snapshots.dropped_aggregate,EXCLUDED.dropped_aggregate),
		dropped_raw=GREATEST(node_observed_snapshots.dropped_raw,EXCLUDED.dropped_raw)
		WHERE EXCLUDED.observed_at > node_observed_snapshots.observed_at
		RETURNING 1
	), security AS (
		INSERT INTO telemetry_security_events(event_id,node_id,observed_at,severity,event_type,detail)
		SELECT e.id,$2,e.observed_at,e.severity,e.event_type,e.detail
		FROM batch CROSS JOIN unnest($20::uuid[],$21::timestamptz[],$22::text[],$23::text[],$24::jsonb[])
		AS e(id,observed_at,severity,event_type,detail)
		WHERE true ON CONFLICT DO NOTHING
	)
	SELECT EXISTS(SELECT 1 FROM batch), EXISTS(SELECT 1 FROM snapshot)`,
		id, node, sequence, kind, snapshot.ObservedAt, size,
		snapshot.BootID, snapshot.AgentInstance, snapshot.AgentVersion, snapshot.OcservVersion, snapshot.OSRelease,
		snapshot.Ocserv, snapshot.System, snapshot.Path,
		snapshot.Security, snapshot.Health, snapshot.Aggregate, snapshot.Raw, snapshot.Architecture,
		ids, times, severities, types, details).Scan(&inserted, &updated)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || pgErr.Code == "54001") {
		return false, false, fmt.Errorf("%w: JSON document is not PostgreSQL-compatible", telemetrystore.ErrInvalidJSON)
	}
	return
}

func (s telemetryStore) ReplaceSessions(ctx context.Context, node uuid.UUID, observed time.Time, sessions []telemetrystore.Session) error {
	if _, err := s.tx.Exec(ctx, `DELETE FROM node_sessions WHERE node_id=$1`, node); err != nil {
		return err
	}
	if len(sessions) == 0 {
		return s.closeAbsentSessions(ctx, node, observed)
	}
	ids := make([]string, len(sessions))
	usernames := make([]string, len(sessions))
	ips := make([]string, len(sessions))
	vpnIPs := make([]string, len(sessions))
	connectedAt := make([]time.Time, len(sessions))
	bytesIn := make([]int64, len(sessions))
	bytesOut := make([]int64, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
		usernames[i] = session.Username
		ips[i] = session.ClientIP
		vpnIPs[i] = session.VPNIP
		connectedAt[i] = session.ConnectedAt
		bytesIn[i] = session.BytesIn
		bytesOut[i] = session.BytesOut
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO node_sessions (node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at,vpn_ip)
		SELECT $1,u.session_id,u.username,u.client_ip::inet,u.connected_at,u.bytes_in,u.bytes_out,$8,NULLIF(u.vpn_ip,'')::inet
		FROM unnest($2::text[],$3::text[],$4::text[],$5::timestamptz[],$6::bigint[],$7::bigint[],$9::text[]) AS u(session_id,username,client_ip,connected_at,bytes_in,bytes_out,vpn_ip)`,
		node, ids, usernames, ips, connectedAt, bytesIn, bytesOut, observed, vpnIPs)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO vpn_session_history
		(node_id, session_id, username, client_ip, connected_at, last_seen_at, bytes_in, bytes_out,vpn_ip)
		SELECT node_id, session_id, username, client_ip, connected_at, observed_at, bytes_in, bytes_out,vpn_ip
		FROM node_sessions WHERE node_id=$1
        -- Older Agents used the snapshot time as the start on every heartbeat.
        AND (vpn_ip IS NOT NULL OR connected_at <> observed_at)
		ON CONFLICT (node_id, session_id, connected_at) DO UPDATE SET
		last_seen_at=excluded.last_seen_at, absent_at=NULL, client_ip=excluded.client_ip, vpn_ip=COALESCE(excluded.vpn_ip,vpn_session_history.vpn_ip),
		bytes_in=excluded.bytes_in, bytes_out=excluded.bytes_out`, node)
	if err != nil {
		return err
	}
	return s.closeAbsentSessions(ctx, node, observed)
}

// absent_at bounds when the session disappeared. A snapshot cannot establish its exact logout time.
func (s telemetryStore) closeAbsentSessions(ctx context.Context, node uuid.UUID, observed time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE vpn_session_history SET absent_at=$2
		WHERE node_id=$1 AND absent_at IS NULL AND last_seen_at<$2`, node, observed)
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
	for i, user := range users {
		if user.Revision > math.MaxInt64 {
			return database.ErrConstraint
		}
		usernames[i] = user.Username
		enabled[i] = user.Enabled
		revisions[i] = int64(user.Revision)
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,observed_at)
		SELECT $1,u.username,u.enabled,u.revision,$5
		FROM unnest($2::text[],$3::boolean[],$4::bigint[]) AS u(username,enabled,revision)`,
		node, usernames, enabled, revisions, observed)
	return err
}

func (s telemetryStore) Activate(ctx context.Context, node uuid.UUID, observed time.Time) error {
	_, err := s.tx.Exec(ctx, `UPDATE nodes SET status='active',updated_at=GREATEST(updated_at,$2),version=version+1 WHERE id=$1 AND status='offline'`, node, observed)
	return err
}

func (s telemetryStore) LockOfflineCandidates(ctx context.Context, before time.Time) ([]uuid.UUID, error) {
	return collectColumn[uuid.UUID](ctx, s.tx.store, `SELECT n.id FROM nodes n JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.status='active' AND o.last_heartbeat_at < $1 FOR UPDATE OF n SKIP LOCKED`, before)
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
		query.Grow(72 + len(chunk)*24)
		query.WriteString(`INSERT INTO observed_groups(node_id,group_name,members,revision,observed_at) VALUES `)
		args := make([]any, 1, 1+len(chunk)*4)
		args[0] = node
		for i, g := range chunk {
			if i > 0 {
				query.WriteByte(',')
			}
			base := 2 + i*4
			fmt.Fprintf(&query, "($1,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3)
			args = append(args, g.Name, g.Members, g.Revision, g.ObservedAt)
		}
		if _, err := s.tx.Exec(ctx, query.String(), args...); err != nil {
			return err
		}
	}
	return nil
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

func (s telemetryStore) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since time.Time) ([]telemetrystore.Point, error) {
	if since.IsZero() {
		return nil, database.ErrConstraint
	}
	query := `SELECT sampled_at,metric,1,value,value,value FROM telemetry_samples WHERE node_id=$1 AND metric=$2 AND sampled_at>=$3 ORDER BY sampled_at LIMIT 2000`
	switch resolution {
	case "raw":
	case "5m", "1h":
		query = fmt.Sprintf(`SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_%s WHERE node_id=$1 AND metric=$2 AND bucket_at >= $3 ORDER BY bucket_at LIMIT 2000`, resolution)
	default:
		return nil, database.ErrConstraint
	}
	return collectRows(ctx, s.tx.store, query, func(r Rows) (p telemetrystore.Point, err error) {
		err = r.Scan(&p.At, &p.Metric, &p.Count, &p.Minimum, &p.Maximum, &p.Average)
		return
	}, nodeID, metric, since)
}

func (s telemetryStore) EnsureUpcomingPartitions(ctx context.Context) error {
	_, err := s.tx.Exec(ctx, `SELECT telemetry_ensure_upcoming_partitions()`)
	return err
}

func (s telemetryStore) MaintainHistory(ctx context.Context, now time.Time) error {
	// The trigger locks every affected queue key, even when it was already
	// dirty. Read raw data in a fresh statement after locking the keys, so
	// samples committed during selection are either included or leave a mark.
	var keys []byte
	if err := s.tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(d),'[]'::jsonb) FROM (
		SELECT bucket_at,node_id,metric FROM telemetry_rollup_queue
		ORDER BY bucket_at,node_id,metric LIMIT 4096 FOR UPDATE SKIP LOCKED
	) d`).Scan(&keys); err != nil {
		return err
	}
	if string(keys) != "[]" {
		if err := s.refreshRollups(ctx, keys); err != nil {
			return err
		}
	}
	if _, err := s.tx.Exec(ctx, `SELECT telemetry_prune_rollups($1)`, now); err != nil {
		return err
	}
	_, err := s.tx.Exec(ctx, `SELECT prune_vpn_history()`)
	return err
}

func (s telemetryStore) refreshRollups(ctx context.Context, keys []byte) error {
	_, err := s.tx.Exec(ctx, `WITH dirty AS (
		SELECT * FROM jsonb_to_recordset($1::jsonb) AS d(bucket_at timestamptz,node_id uuid,metric text)
	), consumed AS (
		DELETE FROM telemetry_rollup_queue q USING dirty d
		WHERE (q.bucket_at,q.node_id,q.metric)=(d.bucket_at,d.node_id,d.metric)
		RETURNING q.bucket_at,q.node_id,q.metric
	), fresh AS (
		INSERT INTO telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value)
		SELECT d.node_id,d.metric,d.bucket_at,count(*),min(s.value),max(s.value),avg(s.value)
		FROM consumed d CROSS JOIN LATERAL (
			SELECT value FROM telemetry_samples
			WHERE node_id=d.node_id AND metric=d.metric
			AND sampled_at BETWEEN d.bucket_at AND d.bucket_at+interval '5 minutes'-interval '1 microsecond'
		) s GROUP BY 1,2,3
		ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value
		RETURNING node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value
	), affected_hours AS (
		SELECT DISTINCT node_id,metric,date_bin(interval '1 hour',bucket_at,'2000-01-01 00:00:00+00'::timestamptz) bucket_at FROM fresh
	), hour_input AS (
		SELECT * FROM fresh
		UNION ALL
		SELECT r.node_id,r.metric,r.bucket_at,r.sample_count,r.min_value,r.max_value,r.avg_value
		FROM affected_hours h JOIN telemetry_rollups_5m r ON r.node_id=h.node_id AND r.metric=h.metric
			AND r.bucket_at BETWEEN h.bucket_at AND h.bucket_at+interval '1 hour'-interval '1 microsecond'
		WHERE NOT EXISTS (SELECT 1 FROM fresh f WHERE (f.node_id,f.metric,f.bucket_at)=(r.node_id,r.metric,r.bucket_at))
	)
	INSERT INTO telemetry_rollups_1h(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value)
	SELECT node_id,metric,date_bin(interval '1 hour',bucket_at,'2000-01-01 00:00:00+00'::timestamptz),
		sum(sample_count),min(min_value),max(max_value),sum(avg_value::numeric*sample_count)/sum(sample_count)
	FROM hour_input GROUP BY 1,2,3
	ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value`, keys)
	return err
}

var _ telemetrystore.Store = telemetryStore{}

func (b *Backend) ValidateTelemetryRuntime(ctx context.Context) error {
	var safe bool
	err := b.QueryRow(ctx, `SELECT NOT has_schema_privilege(current_user,'public','CREATE')
		AND NOT has_table_privilege(current_user,'telemetry_rollups_5m','DELETE,TRUNCATE')
		AND NOT has_table_privilege(current_user,'telemetry_rollups_1h','DELETE,TRUNCATE')
		AND NOT has_table_privilege(current_user,'vpn_network_history','DELETE,TRUNCATE')
		AND NOT has_table_privilege(current_user,'vpn_session_events','DELETE,TRUNCATE')
		AND has_function_privilege(current_user,'prune_vpn_history()','EXECUTE')
		AND has_function_privilege(current_user,'telemetry_prune_rollups(timestamptz)','EXECUTE')
		AND has_function_privilege(current_user,'telemetry_ensure_upcoming_partitions()','EXECUTE')
		AND NOT pg_has_role(current_user,(SELECT relowner FROM pg_class WHERE oid='public.telemetry_samples'::regclass),'USAGE')
		AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb
		FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&safe)
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
	const requiredMonthsProvisioned = `SELECT NOT EXISTS(SELECT 1 FROM generate_series(date_trunc('month',(now()-interval '14 days') AT TIME ZONE 'UTC'),date_trunc('month',(now()+interval '5 minutes') AT TIME ZONE 'UTC'),interval '1 month') month WHERE NOT EXISTS(SELECT 1 FROM pg_inherits WHERE inhparent='public.telemetry_samples'::regclass AND NOT inhdetachpending AND inhrelid=to_regclass('public.telemetry_samples_'||to_char(month,'YYYYMM'))))`
	if err := b.QueryRow(ctx, requiredMonthsProvisioned).Scan(&safe); err != nil {
		return err
	}
	if safe {
		return nil
	}
	// A stopped Controller can outlive its pre-created partitions. The
	// restricted routine restores current and future coverage at startup.
	if _, err := b.Exec(ctx, `SELECT public.telemetry_ensure_upcoming_partitions()`); err != nil {
		return fmt.Errorf("provision upcoming telemetry partitions: %w", err)
	}
	if err := b.QueryRow(ctx, requiredMonthsProvisioned).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("required telemetry month is not provisioned: %w", database.ErrConstraint)
	}
	return nil
}

// UsageStore borrows a transaction; it cannot open or commit another one.
type UsageStore struct{ tx *transaction }

func NewUsageStore(tx database.Tx) *UsageStore {
	return &UsageStore{tx: database.Must[*transaction](tx)}
}

func (t *transaction) UsageStore() userstore.UsageStore { return &UsageStore{tx: t} }

func (s *UsageStore) LockNode(ctx context.Context, nodeID uuid.UUID) error {
	var id uuid.UUID
	return s.tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR NO KEY UPDATE`, nodeID).Scan(&id)
}

func (s *UsageStore) LockCursors(ctx context.Context, nodeID uuid.UUID, keys []userstore.UsageKey) (map[userstore.UsageKey]userstore.UsageCursor, error) {
	cursors := make(map[userstore.UsageKey]userstore.UsageCursor, len(keys))
	if len(keys) == 0 {
		return cursors, nil
	}
	sessionIDs := make([]string, len(keys))
	connectedAts := make([]time.Time, len(keys))
	for i, key := range keys {
		sessionIDs[i] = key.SessionID
		connectedAts[i] = key.Connected
	}
	err := walkRows(ctx, s.tx.store, `SELECT c.session_id,c.connected_at,c.username,c.rx_bytes,c.tx_bytes,c.observed_at FROM unnest($2::text[],$3::timestamptz[]) AS k(session_id,connected_at) JOIN user_usage_cursors c ON c.node_id=$1 AND c.session_id=k.session_id AND c.connected_at=k.connected_at ORDER BY c.session_id,c.connected_at FOR UPDATE OF c`, func(r Rows) error {
		var cursor userstore.UsageCursor
		if err := r.Scan(&cursor.SessionID, &cursor.Connected, &cursor.Username, &cursor.RXBytes, &cursor.TXBytes, &cursor.ObservedAt); err != nil {
			return err
		}
		cursor.Connected = cursor.Connected.UTC().Truncate(time.Microsecond)
		cursor.ObservedAt = cursor.ObservedAt.UTC().Truncate(time.Microsecond)
		cursors[userstore.UsageKey{SessionID: cursor.SessionID, Connected: cursor.Connected}] = cursor
		return nil
	}, nodeID, sessionIDs, connectedAts)
	if err != nil {
		return nil, err
	}
	return cursors, nil
}

func (s *UsageStore) PutCursors(ctx context.Context, nodeID uuid.UUID, cursors []userstore.UsageCursor) error {
	if len(cursors) == 0 {
		return nil
	}
	sessionIDs := make([]string, len(cursors))
	connectedAts := make([]time.Time, len(cursors))
	usernames := make([]string, len(cursors))
	rxBytes := make([]int64, len(cursors))
	txBytes := make([]int64, len(cursors))
	observedAts := make([]time.Time, len(cursors))
	for i, cursor := range cursors {
		sessionIDs[i] = cursor.SessionID
		connectedAts[i] = cursor.Connected
		usernames[i] = cursor.Username
		rxBytes[i] = cursor.RXBytes
		txBytes[i] = cursor.TXBytes
		observedAts[i] = cursor.ObservedAt
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO user_usage_cursors(node_id,session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) SELECT $1,u.session_id,u.connected_at,u.username,u.rx_bytes,u.tx_bytes,u.observed_at FROM unnest($2::text[],$3::timestamptz[],$4::text[],$5::bigint[],$6::bigint[],$7::timestamptz[]) AS u(session_id,connected_at,username,rx_bytes,tx_bytes,observed_at) ON CONFLICT(node_id,session_id,connected_at) DO UPDATE SET username=EXCLUDED.username,rx_bytes=EXCLUDED.rx_bytes,tx_bytes=EXCLUDED.tx_bytes,observed_at=EXCLUDED.observed_at WHERE EXCLUDED.observed_at>user_usage_cursors.observed_at`, nodeID, sessionIDs, connectedAts, usernames, rxBytes, txBytes, observedAts)
	return err
}

func (s *UsageStore) AddUsageBatch(ctx context.Context, nodeID uuid.UUID, deltas []userstore.UsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	usernames := make([]string, len(deltas))
	periods := make([]string, len(deltas))
	periodStarts := make([]time.Time, len(deltas))
	rxBytes := make([]int64, len(deltas))
	txBytes := make([]int64, len(deltas))
	observedAts := make([]time.Time, len(deltas))
	for i, delta := range deltas {
		usernames[i] = delta.Username
		periods[i] = delta.Period
		periodStarts[i] = delta.PeriodStart
		rxBytes[i] = delta.RXBytes
		txBytes[i] = delta.TXBytes
		observedAts[i] = delta.ObservedAt
	}
	_, err := s.tx.Exec(ctx, `INSERT INTO observed_user_usage(node_id,username,period,period_start,rx_bytes,tx_bytes,observed_at) SELECT $1,u.username,u.period,u.period_start,u.rx_bytes,u.tx_bytes,u.observed_at FROM unnest($2::text[],$3::text[],$4::timestamptz[],$5::bigint[],$6::bigint[],$7::timestamptz[]) AS u(username,period,period_start,rx_bytes,tx_bytes,observed_at) ON CONFLICT(node_id,username,period,period_start) DO UPDATE SET rx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.rx_bytes::numeric+EXCLUDED.rx_bytes::numeric)::bigint,tx_bytes=LEAST(9223372036854775807::numeric,observed_user_usage.tx_bytes::numeric+EXCLUDED.tx_bytes::numeric)::bigint,observed_at=GREATEST(observed_user_usage.observed_at,EXCLUDED.observed_at)`, nodeID, usernames, periods, periodStarts, rxBytes, txBytes, observedAts)
	return err
}

var _ userstore.UsageStore = (*UsageStore)(nil)
