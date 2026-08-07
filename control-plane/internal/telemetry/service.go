package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

const (
	MaxBatchBytes       = 512 << 10
	MaxManagedResources = 384
	MaxReportedGroups   = MaxManagedResources * 2
	OfflineAfter        = 90 * time.Second
)

var allowedMetrics = map[string]bool{
	"cpu_usage_ratio": true, "memory_used_bytes": true,
	"network_rx_bytes": true, "network_tx_bytes": true,
	"session_count": true, "connection_rtt_ms": true,
}

type Snapshot struct {
	ObservedAt    time.Time       `json:"observed_at"`
	BootID        string          `json:"boot_id"`
	AgentInstance uuid.UUID       `json:"agent_instance_id"`
	AgentVersion  string          `json:"agent_version"`
	OcservVersion string          `json:"ocserv_version"`
	OSRelease     string          `json:"os_release"`
	Ocserv        json.RawMessage `json:"ocserv"`
	System        json.RawMessage `json:"system"`
	Path          json.RawMessage `json:"path"`
	Dropped       DropCounters    `json:"dropped"`
}

type DropCounters struct {
	Security  uint64 `json:"security"`
	Health    uint64 `json:"health"`
	Aggregate uint64 `json:"aggregate"`
	Raw       uint64 `json:"raw"`
}

type Session struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	ClientIP    string    `json:"client_ip"`
	ConnectedAt time.Time `json:"connected_at"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
}

type IPBan struct {
	IP               string  `json:"ip"`
	SecondsRemaining *uint64 `json:"seconds_remaining,omitempty"`
}

type User struct {
	Username    string `json:"username"`
	Enabled     bool   `json:"enabled"`
	Revision    uint64 `json:"revision"`
	Fingerprint []byte `json:"fingerprint_sha256"`
}

type Group struct {
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Revision    uint64   `json:"revision"`
	Fingerprint []byte   `json:"fingerprint_sha256"`
}

type Sample struct {
	SampledAt time.Time `json:"sampled_at"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
}

type SecurityEvent struct {
	ID         uuid.UUID       `json:"id"`
	ObservedAt time.Time       `json:"observed_at"`
	Severity   string          `json:"severity"`
	Type       string          `json:"type"`
	Detail     json.RawMessage `json:"detail"`
}

type Batch struct {
	ID       uuid.UUID       `json:"id"`
	NodeID   uuid.UUID       `json:"node_id"`
	Sequence uint64          `json:"sequence"`
	Kind     string          `json:"kind"`
	Snapshot Snapshot        `json:"snapshot"`
	Sessions []Session       `json:"sessions"`
	IPBans   []IPBan         `json:"ip_bans"`
	Samples  []Sample        `json:"samples"`
	Security []SecurityEvent `json:"security_events"`
	Users    []User          `json:"users"`
	Groups   []Group         `json:"groups"`
}

type Node struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Version         int64           `json:"version"`
	TrustStatus     string          `json:"trust_status"`
	ConnectionState string          `json:"connection_state"`
	Freshness       string          `json:"freshness"`
	ObservedAt      *time.Time      `json:"observed_at,omitempty"`
	LastHeartbeatAt *time.Time      `json:"last_heartbeat_at,omitempty"`
	BootID          string          `json:"boot_id,omitempty"`
	AgentInstanceID string          `json:"agent_instance_id,omitempty"`
	AgentVersion    string          `json:"agent_version,omitempty"`
	OcservVersion   string          `json:"ocserv_version,omitempty"`
	OSRelease       string          `json:"os_release,omitempty"`
	Ocserv          json.RawMessage `json:"ocserv,omitempty"`
	System          json.RawMessage `json:"system,omitempty"`
	Path            json.RawMessage `json:"path,omitempty"`
	Dropped         DropCounters    `json:"dropped"`
	SessionCount    int             `json:"session_count"`
}

type HistoryPoint struct {
	At      time.Time `json:"at"`
	Metric  string    `json:"metric"`
	Count   int64     `json:"count"`
	Minimum float64   `json:"minimum"`
	Maximum float64   `json:"maximum"`
	Average float64   `json:"average"`
}

func (s *Service) ListIPBans(ctx context.Context, nodeID uuid.UUID, limit int) ([]IPBan, error) {
	if nodeID == uuid.Nil || limit < 1 || limit > 200 {
		return nil, errors.New("IP ban query is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT host(ip),seconds_remaining FROM node_ip_bans WHERE node_id=$1 ORDER BY ip LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list node IP bans: %w", err)
	}
	defer rows.Close()
	result := []IPBan{}
	for rows.Next() {
		var ban IPBan
		if err := rows.Scan(&ban.IP, &ban.SecondsRemaining); err != nil {
			return nil, err
		}
		result = append(result, ban)
	}
	return result, rows.Err()
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, now: time.Now} }

func (s *Service) IngestWire(ctx context.Context, payload []byte) (bool, error) {
	batch, err := decodeWire(payload)
	if err != nil {
		return false, err
	}
	return s.Ingest(ctx, batch)
}

func decodeWire(payload []byte) (Batch, error) {
	if len(payload) == 0 || len(payload) > MaxBatchBytes {
		return Batch{}, errors.New("telemetry wire batch size invalid")
	}
	var wire agentv1.TelemetryBatch
	if err := proto.Unmarshal(payload, &wire); err != nil {
		return Batch{}, errors.New("telemetry wire protobuf invalid")
	}
	batchID, err := uuid.FromBytes(wire.GetBatchId())
	if err != nil {
		return Batch{}, errors.New("telemetry batch ID invalid")
	}
	nodeID, err := uuid.FromBytes(wire.GetNodeId())
	if err != nil {
		return Batch{}, errors.New("telemetry node ID invalid")
	}
	snapshot := wire.GetSnapshot()
	if snapshot == nil || snapshot.GetObservedAt() == nil || snapshot.GetObservedAt().CheckValid() != nil {
		return Batch{}, errors.New("telemetry observed timestamp invalid")
	}
	instance, err := uuid.FromBytes(snapshot.GetAgentInstanceId())
	if err != nil {
		return Batch{}, errors.New("telemetry agent instance invalid")
	}
	kinds := map[agentv1.TelemetryPriority]string{agentv1.TelemetryPriority_TELEMETRY_PRIORITY_SECURITY: "security", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_CURRENT_HEALTH: "current_health", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_AGGREGATE: "aggregate", agentv1.TelemetryPriority_TELEMETRY_PRIORITY_RAW_HISTORY: "raw_history"}
	kind := kinds[wire.GetPriority()]
	dropped := snapshot.GetDropped()
	batch := Batch{ID: batchID, NodeID: nodeID, Sequence: wire.GetSequence(), Kind: kind, Snapshot: Snapshot{ObservedAt: snapshot.GetObservedAt().AsTime(), BootID: snapshot.GetBootId(), AgentInstance: instance, AgentVersion: snapshot.GetAgentVersion(), OcservVersion: snapshot.GetOcservVersion(), OSRelease: snapshot.GetOsRelease(), Ocserv: snapshot.GetOcservJson(), System: snapshot.GetSystemJson(), Path: snapshot.GetPathJson()}}
	if dropped != nil {
		batch.Snapshot.Dropped = DropCounters{Security: dropped.GetSecurity(), Health: dropped.GetHealth(), Aggregate: dropped.GetAggregate(), Raw: dropped.GetRaw()}
	}
	for _, item := range wire.GetSessions() {
		if item.GetConnectedAt() == nil || item.GetConnectedAt().CheckValid() != nil {
			return Batch{}, errors.New("session timestamp invalid")
		}
		if item.GetBytesIn() > math.MaxInt64 || item.GetBytesOut() > math.MaxInt64 {
			return Batch{}, errors.New("session byte count invalid")
		}
		batch.Sessions = append(batch.Sessions, Session{ID: item.GetSessionId(), Username: item.GetUsername(), ClientIP: item.GetClientIp(), ConnectedAt: item.GetConnectedAt().AsTime(), BytesIn: int64(item.GetBytesIn()), BytesOut: int64(item.GetBytesOut())})
	}
	for _, item := range wire.GetIpBans() {
		batch.IPBans = append(batch.IPBans, IPBan{IP: item.GetIp(), SecondsRemaining: item.SecondsRemaining})
	}
	for _, item := range wire.GetUsers() {
		batch.Users = append(batch.Users, User{Username: item.GetUsername(), Enabled: item.GetEnabled(), Revision: item.GetRevision(), Fingerprint: item.GetFingerprintSha256()})
	}
	for _, item := range wire.GetGroups() {
		batch.Groups = append(batch.Groups, Group{Name: item.GetGroupName(), Members: item.GetMembers(), Revision: item.GetRevision(), Fingerprint: item.GetFingerprintSha256()})
	}
	for _, item := range wire.GetSamples() {
		if item.GetSampledAt() == nil || item.GetSampledAt().CheckValid() != nil {
			return Batch{}, errors.New("sample timestamp invalid")
		}
		batch.Samples = append(batch.Samples, Sample{SampledAt: item.GetSampledAt().AsTime(), Metric: item.GetMetric(), Value: item.GetValue()})
	}
	for _, item := range wire.GetSecurityEvents() {
		id, err := uuid.FromBytes(item.GetEventId())
		if err != nil {
			return Batch{}, errors.New("security event ID invalid")
		}
		if item.GetObservedAt() == nil || item.GetObservedAt().CheckValid() != nil {
			return Batch{}, errors.New("security event timestamp invalid")
		}
		batch.Security = append(batch.Security, SecurityEvent{ID: id, ObservedAt: item.GetObservedAt().AsTime(), Severity: item.GetSeverity(), Type: item.GetEventType(), Detail: item.GetDetailJson()})
	}
	return batch, nil
}

func (s *Service) Ingest(ctx context.Context, batch Batch) (bool, error) {
	payload, err := json.Marshal(batch)
	if err != nil || len(payload) > MaxBatchBytes {
		return false, errors.New("telemetry batch exceeds 512 KiB or is invalid")
	}
	if err := validateBatch(batch, s.now()); err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin telemetry ingest: %w", err)
	}
	defer rollback(tx)
	result, err := tx.Exec(ctx, `INSERT INTO telemetry_ingest_batches
		(batch_id,node_id,sequence,kind,observed_at,payload_bytes) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING`, batch.ID, batch.NodeID, batch.Sequence, batch.Kind, batch.Snapshot.ObservedAt, len(payload))
	if err != nil {
		return false, fmt.Errorf("insert telemetry batch: %w", err)
	}
	if result.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}

	updated, err := tx.Exec(ctx, `INSERT INTO node_observed_snapshots
		(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at,dropped_security,dropped_health,dropped_aggregate,dropped_raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$2,$11,$12,$13,$14)
		ON CONFLICT (node_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,received_at=now(),boot_id=EXCLUDED.boot_id,
		agent_instance_id=EXCLUDED.agent_instance_id,agent_version=EXCLUDED.agent_version,ocserv_version=EXCLUDED.ocserv_version,
		os_release=EXCLUDED.os_release,ocserv=EXCLUDED.ocserv,system=EXCLUDED.system,path=EXCLUDED.path,
		last_heartbeat_at=GREATEST(node_observed_snapshots.last_heartbeat_at,EXCLUDED.last_heartbeat_at),
		dropped_security=GREATEST(node_observed_snapshots.dropped_security,EXCLUDED.dropped_security),
		dropped_health=GREATEST(node_observed_snapshots.dropped_health,EXCLUDED.dropped_health),
		dropped_aggregate=GREATEST(node_observed_snapshots.dropped_aggregate,EXCLUDED.dropped_aggregate),
		dropped_raw=GREATEST(node_observed_snapshots.dropped_raw,EXCLUDED.dropped_raw)
		WHERE EXCLUDED.observed_at > node_observed_snapshots.observed_at`,
		batch.NodeID, batch.Snapshot.ObservedAt, batch.Snapshot.BootID, batch.Snapshot.AgentInstance,
		batch.Snapshot.AgentVersion, batch.Snapshot.OcservVersion, batch.Snapshot.OSRelease,
		batch.Snapshot.Ocserv, batch.Snapshot.System, batch.Snapshot.Path,
		batch.Snapshot.Dropped.Security, batch.Snapshot.Dropped.Health, batch.Snapshot.Dropped.Aggregate, batch.Snapshot.Dropped.Raw)
	if err != nil {
		return false, fmt.Errorf("upsert observed snapshot: %w", err)
	}
	if updated.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM node_sessions WHERE node_id=$1`, batch.NodeID); err != nil {
			return false, err
		}
		for _, session := range batch.Sessions {
			if _, err := tx.Exec(ctx, `INSERT INTO node_sessions (node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, batch.NodeID, session.ID, session.Username, session.ClientIP, session.ConnectedAt, session.BytesIn, session.BytesOut, batch.Snapshot.ObservedAt); err != nil {
				return false, fmt.Errorf("insert observed session: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM node_ip_bans WHERE node_id=$1`, batch.NodeID); err != nil {
			return false, err
		}
		for _, ban := range batch.IPBans {
			if _, err := tx.Exec(ctx, `INSERT INTO node_ip_bans (node_id,ip,seconds_remaining,observed_at) VALUES ($1,$2,$3,$4)`, batch.NodeID, ban.IP, ban.SecondsRemaining, batch.Snapshot.ObservedAt); err != nil {
				return false, fmt.Errorf("insert observed IP ban: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM observed_users WHERE node_id=$1`, batch.NodeID); err != nil {
			return false, err
		}
		for _, user := range batch.Users {
			if _, err := tx.Exec(ctx, `INSERT INTO observed_users(node_id,username,enabled,revision,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, batch.NodeID, user.Username, user.Enabled, user.Revision, user.Fingerprint, batch.Snapshot.ObservedAt); err != nil {
				return false, fmt.Errorf("insert observed user: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM observed_groups WHERE node_id=$1`, batch.NodeID); err != nil {
			return false, err
		}
		for _, group := range batch.Groups {
			if _, err := tx.Exec(ctx, `INSERT INTO observed_groups(node_id,group_name,members,revision,fingerprint,observed_at) VALUES($1,$2,$3,$4,$5,$6)`, batch.NodeID, group.Name, group.Members, group.Revision, group.Fingerprint, batch.Snapshot.ObservedAt); err != nil {
				return false, fmt.Errorf("insert observed group: %w", err)
			}
		}
		if batch.Snapshot.ObservedAt.After(s.now().Add(-OfflineAfter)) {
			if _, err := tx.Exec(ctx, `UPDATE nodes SET status='active',updated_at=GREATEST(updated_at,$2),version=version+1 WHERE id=$1 AND status='offline'`, batch.NodeID, batch.Snapshot.ObservedAt); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `SELECT pg_notify('ocservia_outbox',$1)`, batch.NodeID.String()); err != nil {
				return false, err
			}
		}
	}
	for _, event := range batch.Security {
		if _, err := tx.Exec(ctx, `INSERT INTO telemetry_security_events (event_id,node_id,observed_at,severity,event_type,detail) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, event.ID, batch.NodeID, event.ObservedAt, event.Severity, event.Type, event.Detail); err != nil {
			return false, fmt.Errorf("insert security event: %w", err)
		}
	}
	for _, sample := range batch.Samples {
		if _, err := tx.Exec(ctx, `SELECT telemetry_ensure_month_partition($1)`, sample.SampledAt); err != nil {
			return false, fmt.Errorf("ensure telemetry partition: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telemetry_samples (node_id,batch_id,sampled_at,metric,value) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, batch.NodeID, batch.ID, sample.SampledAt, sample.Metric, sample.Value); err != nil {
			return false, fmt.Errorf("insert telemetry sample: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}

func validateBatch(batch Batch, now time.Time) error {
	if batch.ID == uuid.Nil || batch.ID.Version() != 7 || batch.NodeID == uuid.Nil || batch.NodeID.Version() != 7 {
		return errors.New("telemetry IDs must be UUIDv7")
	}
	if batch.Sequence > math.MaxInt64 {
		return errors.New("telemetry sequence exceeds int64")
	}
	if batch.Kind != "security" && batch.Kind != "current_health" && batch.Kind != "aggregate" && batch.Kind != "raw_history" {
		return errors.New("telemetry kind is invalid")
	}
	if batch.Snapshot.ObservedAt.IsZero() || batch.Snapshot.ObservedAt.After(now.Add(5*time.Minute)) || len(batch.Snapshot.BootID) == 0 || len(batch.Snapshot.BootID) > 128 || batch.Snapshot.AgentInstance == uuid.Nil {
		return errors.New("observed snapshot identity or time is invalid")
	}
	for _, value := range []string{batch.Snapshot.AgentVersion, batch.Snapshot.OcservVersion, batch.Snapshot.OSRelease} {
		if len(value) == 0 || len(value) > 128 {
			return errors.New("observed version is invalid")
		}
	}
	for _, document := range []json.RawMessage{batch.Snapshot.Ocserv, batch.Snapshot.System, batch.Snapshot.Path} {
		if !validObject(document) {
			return errors.New("observed documents must be JSON objects")
		}
	}
	if len(batch.Sessions) > 10000 || len(batch.IPBans) > 4096 || len(batch.Samples) > 8192 || len(batch.Security) > 1024 || len(batch.Users) > MaxManagedResources || len(batch.Groups) > MaxReportedGroups {
		return errors.New("telemetry collection count exceeds limit")
	}
	namePattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	for _, user := range batch.Users {
		if !namePattern.MatchString(user.Username) || user.Revision > math.MaxInt64 || len(user.Fingerprint) != sha256.Size {
			return errors.New("user observation is invalid")
		}
	}
	totalMemberships := 0
	for _, group := range batch.Groups {
		totalMemberships += len(group.Members)
		if !namePattern.MatchString(group.Name) || group.Revision > math.MaxInt64 || len(group.Fingerprint) != sha256.Size || len(group.Members) > MaxManagedResources || totalMemberships > MaxManagedResources {
			return errors.New("group observation is invalid")
		}
		copyMembers := slices.Clone(group.Members)
		slices.Sort(copyMembers)
		if !slices.Equal(copyMembers, slices.Compact(copyMembers)) || !slices.Equal(copyMembers, group.Members) {
			return errors.New("group members must be sorted and unique")
		}
		for _, member := range group.Members {
			if !namePattern.MatchString(member) {
				return errors.New("group member observation is invalid")
			}
		}
	}
	for _, ban := range batch.IPBans {
		parsed := net.ParseIP(ban.IP)
		if parsed == nil || parsed.String() != ban.IP {
			return errors.New("IP ban observation is invalid")
		}
	}
	for _, session := range batch.Sessions {
		if session.ID == "" || len(session.ID) > 256 || session.Username == "" || len(session.Username) > 256 || net.ParseIP(session.ClientIP) == nil || session.BytesIn < 0 || session.BytesOut < 0 || session.ConnectedAt.IsZero() {
			return errors.New("session observation is invalid")
		}
	}
	for _, sample := range batch.Samples {
		if !allowedMetrics[sample.Metric] || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.SampledAt.IsZero() || sample.SampledAt.After(now.Add(5*time.Minute)) {
			return errors.New("telemetry sample is invalid")
		}
	}
	for _, event := range batch.Security {
		if event.ID == uuid.Nil || event.ID.Version() != 7 || (event.Severity != "info" && event.Severity != "warning" && event.Severity != "critical") || event.Type == "" || len(event.Type) > 128 || !validObject(event.Detail) {
			return errors.New("security telemetry is invalid")
		}
	}
	return nil
}

func validObject(value json.RawMessage) bool {
	var object map[string]any
	return len(value) > 0 && json.Unmarshal(value, &object) == nil && object != nil
}

func (s *Service) ListNodes(ctx context.Context, after uuid.UUID, limit int) ([]Node, bool, error) {
	return s.ListNodesInWorkspace(ctx, uuid.Nil, after, limit)
}

func (s *Service) ListNodesInWorkspace(ctx context.Context, workspaceID, after uuid.UUID, limit int) ([]Node, bool, error) {
	if limit < 1 || limit > 200 {
		return nil, false, errors.New("node page size must be between 1 and 200")
	}
	var cursor any
	if after != uuid.Nil {
		cursor = after
	}
	rows, err := s.pool.Query(ctx, `SELECT n.id::text,n.name,n.version,n.status,o.observed_at,o.last_heartbeat_at,o.boot_id,o.agent_instance_id::text,o.agent_version,o.ocserv_version,o.os_release,o.ocserv,o.system,o.path,o.dropped_security,o.dropped_health,o.dropped_aggregate,o.dropped_raw,(SELECT count(*) FROM node_sessions ss WHERE ss.node_id=n.id) FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE ($1::uuid IS NULL OR n.id>$1) AND ($3::uuid IS NULL OR n.workspace_id=$3) ORDER BY n.id LIMIT $2`, cursor, limit+1, nullableWorkspace(workspaceID))
	if err != nil {
		return nil, false, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	result := []Node{}
	for rows.Next() {
		node, err := scanNode(rows, s.now())
		if err != nil {
			return nil, false, err
		}
		result = append(result, node)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func nullableWorkspace(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func (s *Service) GetNode(ctx context.Context, id uuid.UUID) (Node, error) {
	row := s.pool.QueryRow(ctx, `SELECT n.id::text,n.name,n.version,n.status,o.observed_at,o.last_heartbeat_at,o.boot_id,o.agent_instance_id::text,o.agent_version,o.ocserv_version,o.os_release,o.ocserv,o.system,o.path,o.dropped_security,o.dropped_health,o.dropped_aggregate,o.dropped_raw,(SELECT count(*) FROM node_sessions ss WHERE ss.node_id=n.id) FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.id=$1`, id)
	return scanNode(row, s.now())
}

type scanner interface{ Scan(...any) error }

func scanNode(row scanner, now time.Time) (Node, error) {
	var n Node
	var observed, heartbeat *time.Time
	var boot, instance, agent, ocserv, os *string
	var ocservJSON, systemJSON, pathJSON []byte
	var ds, dh, da, dr *int64
	if err := row.Scan(&n.ID, &n.Name, &n.Version, &n.TrustStatus, &observed, &heartbeat, &boot, &instance, &agent, &ocserv, &os, &ocservJSON, &systemJSON, &pathJSON, &ds, &dh, &da, &dr, &n.SessionCount); err != nil {
		return Node{}, err
	}
	n.ObservedAt = observed
	n.LastHeartbeatAt = heartbeat
	if heartbeat == nil {
		n.Freshness = "never"
		n.ConnectionState = "offline"
	} else {
		age := now.Sub(*heartbeat)
		if age <= OfflineAfter {
			n.Freshness = "fresh"
			n.ConnectionState = "online"
		} else {
			n.Freshness = "stale"
			n.ConnectionState = "offline"
		}
	}
	if boot != nil {
		n.BootID = *boot
	}
	if instance != nil {
		n.AgentInstanceID = *instance
	}
	if agent != nil {
		n.AgentVersion = *agent
	}
	if ocserv != nil {
		n.OcservVersion = *ocserv
	}
	if os != nil {
		n.OSRelease = *os
	}
	n.Ocserv = ocservJSON
	n.System = systemJSON
	n.Path = pathJSON
	if ds != nil {
		n.Dropped.Security = uint64(*ds)
	}
	if dh != nil {
		n.Dropped.Health = uint64(*dh)
	}
	if da != nil {
		n.Dropped.Aggregate = uint64(*da)
	}
	if dr != nil {
		n.Dropped.Raw = uint64(*dr)
	}
	return n, nil
}

func (s *Service) ListSessions(ctx context.Context, nodeID uuid.UUID, after string, limit int) ([]Session, bool, error) {
	if limit < 1 || limit > 200 || len(after) > 256 {
		return nil, false, errors.New("session page is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT session_id,username,host(client_ip),connected_at,bytes_in,bytes_out FROM node_sessions WHERE node_id=$1 AND session_id>$2 ORDER BY session_id LIMIT $3`, nodeID, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []Session{}
	for rows.Next() {
		var item Session
		if err := rows.Scan(&item.ID, &item.Username, &item.ClientIP, &item.ConnectedAt, &item.BytesIn, &item.BytesOut); err != nil {
			return nil, false, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(result) > limit
	if hasMore {
		result = result[:limit]
	}
	return result, hasMore, nil
}

func (s *Service) History(ctx context.Context, nodeID uuid.UUID, metric, resolution string, since time.Time) ([]HistoryPoint, error) {
	if !allowedMetrics[metric] {
		return nil, errors.New("metric is invalid")
	}
	if since.IsZero() {
		since = s.now().Add(-24 * time.Hour)
	}
	query := `SELECT sampled_at,metric,1,value,value,value FROM telemetry_samples WHERE node_id=$1 AND metric=$2 AND sampled_at >= $3 ORDER BY sampled_at LIMIT 2000`
	if resolution == "5m" {
		query = `SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_5m WHERE node_id=$1 AND metric=$2 AND bucket_at >= $3 ORDER BY bucket_at LIMIT 2000`
	} else if resolution == "1h" {
		query = `SELECT bucket_at,metric,sample_count,min_value,max_value,avg_value FROM telemetry_rollups_1h WHERE node_id=$1 AND metric=$2 AND bucket_at >= $3 ORDER BY bucket_at LIMIT 2000`
	} else if resolution != "raw" {
		return nil, errors.New("resolution is invalid")
	}
	rows, err := s.pool.Query(ctx, query, nodeID, metric, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []HistoryPoint{}
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.At, &p.Metric, &p.Count, &p.Minimum, &p.Maximum, &p.Average); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

func (s *Service) Maintain(ctx context.Context) error {
	now := s.now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT n.id FROM nodes n JOIN node_observed_snapshots o ON o.node_id=n.id WHERE n.status='active' AND o.last_heartbeat_at < ($1::timestamptz - interval '90 seconds') FOR UPDATE OF n SKIP LOCKED`, now)
	if err != nil {
		return err
	}
	var offline []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		offline = append(offline, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range offline {
		if _, err := tx.Exec(ctx, `UPDATE nodes SET status='offline',updated_at=$2,version=version+1 WHERE id=$1 AND status='active'`, id, now); err != nil {
			return err
		}
		eventID := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO transport_events(event_id,node_id,event_type,occurred_at,traceparent,payload) VALUES($1,$2,'disconnected',$3,$4,$5)`, eventID, id, now, newTraceparent(), []byte("heartbeat timeout")); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, table := range []struct{ name, interval string }{{"telemetry_rollups_5m", "5 minutes"}, {"telemetry_rollups_1h", "1 hour"}} {
		_, err := s.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,date_bin($1::interval,sampled_at,'2000-01-01'::timestamptz),count(*),min(value),max(value),avg(value) FROM telemetry_samples WHERE sampled_at >= $2 GROUP BY 1,2,3 ON CONFLICT (node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value`, table.name), table.interval, now.Add(-48*time.Hour))
		if err != nil {
			return err
		}
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM telemetry_rollups_5m WHERE bucket_at < ($1::timestamptz - interval '90 days')`, now); err != nil {
		return err
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM telemetry_rollups_1h WHERE bucket_at < ($1::timestamptz - interval '13 months')`, now); err != nil {
		return err
	}
	return s.dropExpiredRawPartitions(ctx, now.Add(-14*24*time.Hour))
}

func (s *Service) dropExpiredRawPartitions(ctx context.Context, cutoff time.Time) error {
	_, err := s.pool.Exec(ctx, `SELECT telemetry_drop_expired_partitions($1)`, cutoff)
	return err
}

func newTraceparent() string {
	trace := uuid.Must(uuid.NewV7())
	span := uuid.Must(uuid.NewV7())
	return "00-" + hex.EncodeToString(trace[:]) + "-" + hex.EncodeToString(span[:8]) + "-01"
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
