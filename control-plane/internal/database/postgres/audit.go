package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/google/uuid"
)

type auditStore struct{ database.Store }

func (b *Backend) AuditStore() auditstore.Store     { return auditStore{b} }
func (t *transaction) AuditStore() auditstore.Store { return auditStore{t} }
func (s auditStore) Lock(ctx context.Context, workspace uuid.UUID) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspace.String())
	return err
}
func (s auditStore) Clock(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := s.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return at, err
}
func (s auditStore) Previous(ctx context.Context, workspace uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspace).Scan(&hash)
	return hash, err
}
func (s auditStore) Append(ctx context.Context, args []any) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,source_session_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,before_summary,after_summary,error_type,previous_event_hash,event_hash,auth_version,event_key_id,event_mac) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, args...)
	return err
}

const auditEventColumns = `id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash,auth_version,event_key_id,event_mac,details_compacted_at,compaction_key_id,compaction_mac`

func (s auditStore) Events(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT `+auditEventColumns+` FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at,id`, workspace)
}
func (s auditStore) EventsAfter(ctx context.Context, workspace uuid.UUID, afterAt value.Timestamp, afterID uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT `+auditEventColumns+` FROM audit_events WHERE workspace_id=$1 AND (occurred_at,id)>($2,$3) ORDER BY occurred_at,id`, workspace, afterAt, afterID)
}
func (s auditStore) LatestEvent(ctx context.Context, workspace uuid.UUID) (id uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT id,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspace).Scan(&id, &hash)
	return
}
func (s auditStore) VpnAuditRecords(ctx context.Context, q auditstore.VpnAuditFilter) (database.Rows, error) {
	// Every SQL fragment below is constant. User input is bound separately.
	var table, detail, at string
	switch q.Kind {
	case "observations":
		table, at = "vpn_session_history", "connected_at"
		detail = "h.last_seen_at,h.absent_at,'observed'::text,NULL::uuid,NULL::text,NULL::integer,NULL::text,NULL::timestamptz"
	case "sessions":
		table, at = "vpn_session_events", "observed_at"
		detail = "h.observed_at,NULL::timestamptz,h.event_type,h.event_id,NULL::text,NULL::integer,NULL::text,NULL::timestamptz"
	case "network":
		table, at = "vpn_network_history", "observed_at"
		detail = "h.observed_at,NULL::timestamptz,'flow'::text,h.event_id,host(h.destination_ip),h.destination_port,h.protocol,h.flow_started_at"
	default:
		return nil, database.ErrUnsupported
	}
	query := `SELECT h.node_id,h.session_id,h.username,host(h.client_ip),host(h.vpn_ip),h.connected_at,
		h.bytes_in::text,h.bytes_out::text,` + detail + ` FROM ` + table + ` h JOIN nodes n ON n.id=h.node_id
		WHERE n.workspace_id=$1 AND ($2::uuid IS NULL OR h.node_id=$2)
		AND ($3='' OR h.username=$3) AND ($4::timestamptz IS NULL OR h.` + at + ` >= $4)
		AND ($5::timestamptz IS NULL OR h.` + at + ` < $5)`
	key := "event_id"
	if q.Kind == "observations" {
		key = "session_id"
	}
	args := []any{q.Workspace, q.Node, q.Username, q.Since, q.Until, q.Limit + 1}
	if q.After != nil {
		query += " AND (h." + at + ",h.node_id,h." + key + ") < ($7,$8,$9)"
		args = append(args, q.After.At, q.After.Node, q.After.Key)
	}
	if q.Destination != "" {
		args = append(args, q.Destination)
		query += " AND h.destination_ip=$" + strconv.Itoa(len(args)) + "::inet"
	}
	query += " ORDER BY h." + at + " DESC,h.node_id DESC,h." + key + " DESC LIMIT $6"
	return s.Query(ctx, query, args...)
}

func (s auditStore) RecentEvents(ctx context.Context, workspace uuid.UUID, limit int) (database.Rows, error) {
	return s.Query(ctx, `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,result,reason,error_type,previous_event_hash,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT $2`, workspace, limit)
}
func (s auditStore) Checkpoints(ctx context.Context, workspace uuid.UUID) (database.Rows, error) {
	return s.Query(ctx, `SELECT through_event_id,through_event_hash,signature FROM audit_checkpoints WHERE workspace_id=$1 ORDER BY created_at,id`, workspace)
}
func (s auditStore) LatestCheckpoint(ctx context.Context, workspace uuid.UUID) (eventID uuid.UUID, hash, signature []byte, occurredAt value.Timestamp, authVersion int16, err error) {
	err = s.QueryRow(ctx, `SELECT c.through_event_id,c.through_event_hash,c.signature,e.occurred_at,e.auth_version
		FROM audit_checkpoints c JOIN audit_events e ON e.id=c.through_event_id AND e.workspace_id=c.workspace_id AND e.event_hash=c.through_event_hash
		WHERE c.workspace_id=$1 ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, workspace).Scan(&eventID, &hash, &signature, &occurredAt, &authVersion)
	return
}
func (s auditStore) AddCheckpoint(ctx context.Context, id, workspace, event uuid.UUID, hash, signature []byte) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(workspace_id,through_event_id) DO NOTHING`, id, workspace, event, hash, signature)
	return err
}
func (s auditStore) Workspaces(ctx context.Context) (database.Rows, error) {
	return s.Query(ctx, `SELECT w.id FROM workspaces w WHERE EXISTS (SELECT 1 FROM audit_events e WHERE e.workspace_id=w.id) ORDER BY w.id`)
}

func (s auditStore) CheckpointHash(ctx context.Context, workspace, event uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT through_event_hash FROM audit_checkpoints WHERE workspace_id=$1 AND through_event_id=$2`, workspace, event).Scan(&hash)
	return hash, err
}

func (s auditStore) ExpiredDetails(ctx context.Context, cutoff value.Timestamp) (database.Rows, error) {
	return s.Query(ctx, `SELECT workspace_id,id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash,auth_version,event_key_id,event_mac,details_compacted_at,compaction_key_id,compaction_mac FROM audit_events WHERE details_compacted_at IS NULL AND auth_version=1 AND action<>'audit.auth.transition' AND occurred_at<$1 ORDER BY occurred_at,id LIMIT 32 FOR UPDATE SKIP LOCKED`, cutoff)
}
func (s auditStore) CompactDetails(ctx context.Context, id uuid.UUID, hash []byte, key string, mac []byte, at value.Timestamp) error {
	var changed bool
	err := s.QueryRow(ctx, `SELECT audit_compact_detail($1,$2,$3,$4,$5)`, id, hash, key, mac, at).Scan(&changed)
	if err == nil && !changed {
		return database.ErrNotFound
	}
	return err
}

func (s auditStore) CompactSecurity(ctx context.Context, cutoff value.Timestamp) error {
	_, err := s.Exec(ctx, `SELECT security_compact_details($1)`, cutoff)
	return err
}

type historyRetentionStore struct{ database.Tx }

func (t *transaction) HistoryRetentionStore() historyretention.Store { return historyRetentionStore{t} }

const retentionEligible = `c.details_compacted_at IS NULL AND c.payload_type<>'config_plan' AND c.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND p.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND c.updated_at<$1 AND c.expires_at<$1 AND p.completed_at<$1
 AND o.published_at IS NOT NULL AND o.locked_by IS NULL
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=c.id AND a.state IN ('sending','unknown'))
 AND NOT EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='unknown')
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=p.id AND u.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('consumed','expired','revoked','failed'))`

func (s historyRetentionStore) Candidates(ctx context.Context, cutoff value.Timestamp) ([]uuid.UUID, error) {
	return database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT c.id FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id WHERE `+retentionEligible+` ORDER BY c.updated_at,c.id LIMIT 32 FOR UPDATE OF o SKIP LOCKED`, cutoff))
}
func (s historyRetentionStore) LockCommand(ctx context.Context, id uuid.UUID, cutoff value.Timestamp) (v historyretention.Command, err error) {
	err = s.QueryRow(ctx, `SELECT c.id,c.node_id,c.envelope FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id WHERE c.details_compacted_at IS NULL AND c.payload_type<>'config_plan' AND c.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND p.state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')
 AND c.updated_at<$1 AND c.expires_at<$1 AND p.completed_at<$1
 AND o.published_at IS NOT NULL AND o.locked_by IS NULL
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.command_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM command_attempts a WHERE a.command_id=c.id AND a.state IN ('sending','unknown'))
 AND NOT EXISTS(SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id AND r.state='unknown')
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.operation_id=p.id AND u.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.operation_id=p.id AND a.state NOT IN ('consumed','expired','revoked','failed')) AND c.id=$2 FOR UPDATE OF c,p`, cutoff, id).Scan(&v.ID, &v.NodeID, &v.Envelope)
	return
}
func (s historyRetentionStore) Compact(ctx context.Context, c historyretention.Command, header, digest []byte, at value.Timestamp) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,envelope_sha256=$3,details_compacted_at=$4 WHERE id=$1`, c.ID, header, digest, at); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,last_error=NULL WHERE command_id=$1`, c.ID, header); err != nil {
		return err
	}
	return nil
}

func (s historyRetentionStore) CompactRetired(ctx context.Context, cutoff value.Timestamp) error {
	ids, err := database.CollectColumn[uuid.UUID](s.Query(ctx, `SELECT n.id FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.status='revoked' AND k.state='revoked' AND k.revoked_at<$1
 AND NOT EXISTS(SELECT 1 FROM commands c WHERE c.node_id=n.id AND c.state IN ('queued','dispatched','accepted','running','unknown'))
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=n.id)
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.node_id=n.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.state NOT IN ('succeeded','failed','rolled_back'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.node_id=n.id AND a.state NOT IN ('consumed','expired','revoked','failed'))
 AND (EXISTS(SELECT 1 FROM node_sessions s WHERE s.node_id=n.id) OR EXISTS(SELECT 1 FROM node_observed_snapshots s WHERE s.node_id=n.id AND (s.ocserv<>'{}' OR s.system<>'{}' OR s.path<>'{}'))) ORDER BY k.revoked_at,n.id LIMIT 32 FOR UPDATE OF n,k SKIP LOCKED`, cutoff))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Exec(ctx, `WITH retired AS (SELECT node_id,session_id FROM node_sessions WHERE node_id=$1 ORDER BY session_id LIMIT 32 FOR UPDATE SKIP LOCKED) DELETE FROM node_sessions s USING retired r WHERE s.node_id=r.node_id AND s.session_id=r.session_id`, id); err != nil {
			return err
		}
		if _, err := s.Exec(ctx, `UPDATE node_observed_snapshots SET ocserv='{}',system='{}',path='{}' WHERE node_id=$1`, id); err != nil {
			return err
		}
	}
	return nil
}
