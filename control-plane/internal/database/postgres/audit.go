package postgres

import (
	"context"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit/auditstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/historyretention"
	"github.com/google/uuid"
)

type auditStore struct{ store }

func (b *Backend) AuditStore() auditstore.Store     { return auditStore{b.store} }
func (t *transaction) AuditStore() auditstore.Store { return auditStore{t.store} }
func (s auditStore) Lock(ctx context.Context, workspace uuid.UUID) error {
	_, err := s.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, workspace.String())
	return err
}
func (s auditStore) Clock(ctx context.Context) (time.Time, error) {
	return wallTime(ctx, s.store)
}
func (s auditStore) Previous(ctx context.Context, workspace uuid.UUID) ([]byte, error) {
	var hash []byte
	err := s.QueryRow(ctx, `SELECT event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspace).Scan(&hash)
	return hash, err
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func (s auditStore) Append(ctx context.Context, v auditstore.EventRow) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_events (id,workspace_id,occurred_at,actor_type,actor_id,source_session_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,approval_id,result,reason,before_summary,after_summary,error_type,previous_event_hash,event_hash,auth_version,event_key_id,event_mac) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		v.ID, v.WorkspaceID, v.At, v.ActorType, v.ActorID, v.SessionID, v.Action, v.ResourceType, nullableUUID(v.ResourceID), v.NodeID, v.RequestID, nullableText(v.TraceID), v.CommandID, v.ApprovalID, v.Result, nullableText(v.Reason), nullableBytes(v.BeforeSummary), nullableBytes(v.AfterSummary), nullableText(v.ErrorType), v.PreviousHash, v.EventHash, v.AuthVersion, v.EventKeyID, v.EventMAC)
	return err
}

const auditEventColumns = `id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,request_id,COALESCE(trace_id,''),result,COALESCE(reason,''),source_session_id,node_id,command_id,approval_id,before_summary,after_summary,COALESCE(error_type,''),previous_event_hash,event_hash,auth_version,event_key_id,event_mac,details_compacted_at,compaction_key_id,compaction_mac`

func scanAuditEventRow(r Rows, workspace uuid.UUID, includeWorkspace bool) (v auditstore.EventRow, err error) {
	v.WorkspaceID = workspace
	var resourceID *uuid.UUID
	fields := []any{
		&v.ID, &v.At, &v.ActorType, &v.ActorID, &v.Action,
		&v.ResourceType, &resourceID, &v.RequestID, &v.TraceID, &v.Result,
		&v.Reason, &v.SessionID, &v.NodeID, &v.CommandID, &v.ApprovalID,
		&v.BeforeSummary, &v.AfterSummary, &v.ErrorType, &v.PreviousHash, &v.EventHash, &v.AuthVersion,
		&v.EventKeyID, &v.EventMAC, &v.CompactedAt, &v.CompactionKeyID, &v.CompactionMAC,
	}
	if includeWorkspace {
		fields = append([]any{&v.WorkspaceID}, fields...)
	}
	if err = r.Scan(fields...); err != nil {
		return auditstore.EventRow{}, err
	}
	v.At = v.At.UTC().Truncate(time.Microsecond)
	if resourceID != nil {
		v.ResourceID = *resourceID
	}
	return v, nil
}

func (s auditStore) walkEventRows(ctx context.Context, workspace uuid.UUID, query string, visit func(auditstore.EventRow) error, args ...any) error {
	return walkRows(ctx, s.store, query, func(r Rows) error {
		v, err := scanAuditEventRow(r, workspace, false)
		if err != nil {
			return err
		}
		return visit(v)
	}, args...)
}

func (s auditStore) Events(ctx context.Context, workspace uuid.UUID, visit func(auditstore.EventRow) error) error {
	return s.walkEventRows(ctx, workspace, `SELECT `+auditEventColumns+` FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at,id`, visit, workspace)
}
func (s auditStore) EventsFrom(ctx context.Context, workspace uuid.UUID, afterAt time.Time, afterID uuid.UUID, visit func(auditstore.EventRow) error) error {
	return s.walkEventRows(ctx, workspace, `SELECT `+auditEventColumns+` FROM audit_events WHERE workspace_id=$1 AND (occurred_at,id)>=($2,$3) ORDER BY occurred_at,id`, visit, workspace, afterAt, afterID)
}
func (s auditStore) LatestEvent(ctx context.Context, workspace uuid.UUID) (id uuid.UUID, hash []byte, err error) {
	err = s.QueryRow(ctx, `SELECT id,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT 1`, workspace).Scan(&id, &hash)
	return
}
func (s auditStore) VpnAuditRecords(ctx context.Context, q auditstore.VpnAuditFilter) ([]auditstore.VpnAuditRecord, error) {
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
	return collectRows(ctx, s.store, query, func(r Rows) (v auditstore.VpnAuditRecord, err error) {
		err = r.Scan(&v.NodeID, &v.SessionID, &v.Username, &v.ClientIP, &v.VPNIP, &v.ConnectedAt, &v.BytesIn, &v.BytesOut, &v.LastSeenAt, &v.AbsentAt, &v.EventType, &v.EventID, &v.DestinationIP, &v.DestinationPort, &v.Protocol, &v.FlowStartedAt)
		return
	}, args...)
}

func (s auditStore) RecentEvents(ctx context.Context, workspace uuid.UUID, limit int) ([]auditstore.RecentEvent, error) {
	return collectRows(ctx, s.store, `SELECT id,occurred_at,actor_type,actor_id,action,resource_type,resource_id,node_id,request_id,trace_id,command_id,result,reason,error_type,previous_event_hash,event_hash FROM audit_events WHERE workspace_id=$1 ORDER BY occurred_at DESC,id DESC LIMIT $2`, func(r Rows) (v auditstore.RecentEvent, err error) {
		var previous, hash []byte
		if err = r.Scan(&v.ID, &v.OccurredAt, &v.ActorType, &v.ActorID, &v.Action, &v.ResourceType, &v.ResourceID, &v.NodeID, &v.RequestID, &v.TraceID, &v.CommandID, &v.Result, &v.Reason, &v.ErrorType, &previous, &hash); err != nil {
			return auditstore.RecentEvent{}, err
		}
		v.PreviousEventHash = hex.EncodeToString(previous)
		v.EventHash = hex.EncodeToString(hash)
		return v, nil
	}, workspace, limit)
}
func (s auditStore) LatestCheckpoint(ctx context.Context, workspace uuid.UUID) (eventID uuid.UUID, hash, signature []byte, occurredAt time.Time, authVersion int16, err error) {
	err = s.QueryRow(ctx, `WITH latest AS (
		SELECT workspace_id,through_event_id,through_event_hash,signature FROM audit_checkpoints
		WHERE workspace_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1
	) SELECT c.through_event_id,c.through_event_hash,c.signature,COALESCE(e.occurred_at,'0001-01-01 UTC'::timestamptz),COALESCE(e.auth_version,0)
		FROM latest c LEFT JOIN audit_events e ON e.id=c.through_event_id AND e.workspace_id=c.workspace_id AND e.event_hash=c.through_event_hash`, workspace).Scan(&eventID, &hash, &signature, &occurredAt, &authVersion)
	return
}
func (s auditStore) AddCheckpoint(ctx context.Context, id, workspace, event uuid.UUID, hash, signature []byte) error {
	_, err := s.Exec(ctx, `INSERT INTO audit_checkpoints(id,workspace_id,through_event_id,through_event_hash,signature,created_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(workspace_id,through_event_id) DO NOTHING`, id, workspace, event, hash, signature)
	return err
}
func (s auditStore) Workspaces(ctx context.Context) ([]uuid.UUID, error) {
	return collectColumn[uuid.UUID](ctx, s.store, `SELECT w.id FROM workspaces w WHERE EXISTS (SELECT 1 FROM audit_events e WHERE e.workspace_id=w.id) ORDER BY w.id`)
}

func (s auditStore) ExpiredDetails(ctx context.Context, cutoff time.Time) ([]auditstore.EventRow, error) {
	return collectRows(ctx, s.store, `SELECT workspace_id,`+auditEventColumns+` FROM audit_events WHERE details_compacted_at IS NULL AND auth_version=1 AND occurred_at<$1 ORDER BY occurred_at,id LIMIT 32 FOR UPDATE SKIP LOCKED`, func(r Rows) (auditstore.EventRow, error) {
		return scanAuditEventRow(r, uuid.Nil, true)
	}, cutoff)
}
func (s auditStore) CompactDetails(ctx context.Context, id uuid.UUID, hash []byte, key string, mac []byte, at time.Time) error {
	var changed bool
	err := s.QueryRow(ctx, `SELECT audit_compact_detail($1,$2,$3,$4,$5)`, id, hash, key, mac, at).Scan(&changed)
	if err == nil && !changed {
		return database.ErrNotFound
	}
	return err
}

func (s auditStore) CompactSecurity(ctx context.Context, cutoff time.Time) error {
	_, err := s.Exec(ctx, `SELECT security_compact_details($1)`, cutoff)
	return err
}

type historyRetentionStore struct{ *transaction }

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

func (s historyRetentionStore) Candidates(ctx context.Context, cutoff time.Time) ([]uuid.UUID, error) {
	return collectColumn[uuid.UUID](ctx, s.store, `SELECT c.id FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id WHERE `+retentionEligible+` ORDER BY c.updated_at,c.id LIMIT 32 FOR UPDATE OF o SKIP LOCKED`, cutoff)
}
func (s historyRetentionStore) LockCommand(ctx context.Context, id uuid.UUID, cutoff time.Time) (v historyretention.Command, err error) {
	err = s.QueryRow(ctx, `SELECT c.id,c.node_id,c.envelope FROM outbox_events o JOIN commands c ON c.id=o.command_id JOIN operations p ON p.id=c.operation_id WHERE `+retentionEligible+` AND c.id=$2 FOR UPDATE OF c,p`, cutoff, id).Scan(&v.ID, &v.NodeID, &v.Envelope)
	return
}
func (s historyRetentionStore) Compact(ctx context.Context, c historyretention.Command, header []byte, at time.Time) error {
	if _, err := s.Exec(ctx, `UPDATE commands SET envelope=$2,details_compacted_at=$3 WHERE id=$1`, c.ID, header, at); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, `UPDATE outbox_events SET payload=$2,last_error=NULL WHERE command_id=$1`, c.ID, header); err != nil {
		return err
	}
	return nil
}

func (s historyRetentionStore) CompactRetired(ctx context.Context, cutoff time.Time) error {
	ids, err := collectColumn[uuid.UUID](ctx, s.store, `SELECT n.id FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.status='revoked' AND k.state='revoked' AND k.revoked_at<$1
 AND NOT EXISTS(SELECT 1 FROM commands c WHERE c.node_id=n.id AND c.state IN ('queued','dispatched','accepted','running','unknown'))
 AND NOT EXISTS(SELECT 1 FROM node_command_leases l WHERE l.node_id=n.id)
 AND NOT EXISTS(SELECT 1 FROM config_apply_operations a WHERE a.node_id=n.id AND a.state NOT IN ('succeeded','failed','rolled_back','expired'))
 AND NOT EXISTS(SELECT 1 FROM agent_upgrade_operations u WHERE u.node_id=n.id AND u.state NOT IN ('succeeded','failed','rolled_back'))
 AND NOT EXISTS(SELECT 1 FROM artifact_operations a WHERE a.node_id=n.id AND a.state NOT IN ('consumed','expired','revoked','failed'))
 AND (EXISTS(SELECT 1 FROM node_sessions s WHERE s.node_id=n.id) OR EXISTS(SELECT 1 FROM node_observed_snapshots s WHERE s.node_id=n.id AND (s.ocserv<>'{}' OR s.system<>'{}' OR s.path<>'{}'))) ORDER BY k.revoked_at,n.id LIMIT 32 FOR UPDATE OF n,k SKIP LOCKED`, cutoff)
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
