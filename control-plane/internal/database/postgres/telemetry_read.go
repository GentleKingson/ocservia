package postgres

import (
	"context"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryread"
	"github.com/google/uuid"
)

type telemetryReadStore struct{ tx database.Tx }

func (t *transaction) TelemetryReadStore() telemetryread.Store { return telemetryReadStore{t} }

const telemetryNodeSelect = `SELECT n.id,n.name,n.version,n.status,o.observed_at,o.last_heartbeat_at,COALESCE(o.boot_id,''),COALESCE(o.agent_instance_id::text,''),COALESCE(o.agent_version,''),COALESCE(o.ocserv_version,''),COALESCE(o.os_release,''),COALESCE(o.architecture,''),o.ocserv,o.system,o.path,COALESCE(o.dropped_security,0),COALESCE(o.dropped_health,0),COALESCE(o.dropped_aggregate,0),COALESCE(o.dropped_raw,0),(SELECT count(*) FROM node_sessions ss WHERE ss.node_id=n.id) FROM nodes n LEFT JOIN node_observed_snapshots o ON o.node_id=n.id`

func scanTelemetryNode(row database.Row) (telemetryread.Node, error) {
	var n telemetryread.Node
	err := row.Scan(&n.ID, &n.Name, &n.Version, &n.Status, &n.ObservedAt, &n.Heartbeat, &n.BootID, &n.InstanceID, &n.AgentVersion, &n.OcservVersion, &n.OSRelease, &n.Architecture, &n.Ocserv, &n.System, &n.Path, &n.Security, &n.Health, &n.Aggregate, &n.Raw, &n.Sessions)
	return n, err
}
func (s telemetryReadStore) Nodes(ctx context.Context, workspace, after uuid.UUID, limit int) ([]telemetryread.Node, error) {
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
	nodes := []telemetryread.Node{}
	for rows.Next() {
		n, err := scanTelemetryNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
func (s telemetryReadStore) Node(ctx context.Context, id uuid.UUID) (telemetryread.Node, error) {
	return scanTelemetryNode(s.tx.QueryRow(ctx, telemetryNodeSelect+` WHERE n.id=$1`, id))
}
func (s telemetryReadStore) UpgradeEligibility(ctx context.Context, id uuid.UUID) (bool, error) {
	var capable, conflict bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_capabilities WHERE node_id=$1 AND capability='ocserv.agent.upgrade.v2' AND approved=true), EXISTS(SELECT 1 FROM agent_upgrade_operations WHERE node_id=$1 AND completed_at IS NULL AND state IN ('queued','accepted','running','unknown'))`, id).Scan(&capable, &conflict)
	return capable && !conflict, err
}
func (s telemetryReadStore) Sessions(ctx context.Context, id uuid.UUID, after string, limit int) ([]telemetryread.Session, error) {
	rows, err := s.tx.Query(ctx, `SELECT session_id,username,host(client_ip),connected_at,bytes_in,bytes_out FROM node_sessions WHERE node_id=$1 AND session_id>$2 ORDER BY session_id LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []telemetryread.Session{}
	for rows.Next() {
		var v telemetryread.Session
		if err := rows.Scan(&v.ID, &v.Username, &v.ClientIP, &v.ConnectedAt, &v.BytesIn, &v.BytesOut); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s telemetryReadStore) IPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetryread.IPBan, error) {
	rows, err := s.tx.Query(ctx, `SELECT host(ip),seconds_remaining FROM node_ip_bans WHERE node_id=$1 ORDER BY ip LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []telemetryread.IPBan{}
	for rows.Next() {
		var v telemetryread.IPBan
		if err := rows.Scan(&v.IP, &v.SecondsRemaining); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
