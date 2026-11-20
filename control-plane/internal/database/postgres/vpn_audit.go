package postgres

import (
	"context"
	"time"

	agent "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
)

func (t *transaction) CheckAuditFence(ctx context.Context, node, connection []byte, epoch int64) error {
	var one int
	return t.QueryRow(ctx, `SELECT 1 FROM connection_owner_fencing WHERE node_id=$1 AND connection_id=$2 AND owner_epoch=$3 AND lease_until>clock_timestamp() FOR SHARE`, node, connection, epoch).Scan(&one)
}

func (t *transaction) SaveAuditRecord(ctx context.Context, node uuid.UUID, r *agent.VpnAuditRecord) error {
	var connected *time.Time
	if r.ConnectedMs != 0 {
		v := time.UnixMilli(r.ConnectedMs)
		connected = &v
	}
	if r.EventType == "flow" {
		_, err := t.Exec(ctx, `INSERT INTO vpn_network_history(node_id,event_id,session_id,username,client_ip,vpn_ip,connected_at,observed_at,bytes_in,bytes_out,destination_ip,destination_port,protocol,flow_started_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (node_id,event_id) DO NOTHING`, node, r.EventId, r.SessionId, r.Username, r.ClientIp, r.VpnIp, connected, time.UnixMilli(r.ObservedMs), r.BytesIn, r.BytesOut, r.DestinationIp, r.DestinationPort, r.Protocol, time.UnixMilli(r.FlowStartedMs))
		return err
	}
	_, err := t.Exec(ctx, `INSERT INTO vpn_session_events(node_id,event_id,session_id,username,client_ip,vpn_ip,connected_at,observed_at,bytes_in,bytes_out,event_type)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (node_id,event_id) DO NOTHING`, node, r.EventId, r.SessionId, r.Username, r.ClientIp, r.VpnIp, connected, time.UnixMilli(r.ObservedMs), r.BytesIn, r.BytesOut, r.EventType)
	return err
}
