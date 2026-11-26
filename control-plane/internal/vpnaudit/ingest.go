// Package vpnaudit accepts bounded audit batches from authenticated node connections.
package vpnaudit

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"time"
	"unicode"
	"unicode/utf8"

	agent "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	wire "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	ingress "github.com/GentleKingson/ocservia/control-plane/internal/transportevents/store"
	"github.com/google/uuid"
)

var ErrInvalid = errors.New("invalid VPN audit batch")
var ErrInactive = errors.New("inactive VPN audit connection")

func validText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func ip(s string) bool { a, e := netip.ParseAddr(s); return e == nil && a.Zone() == "" }
func valid(r *agent.VpnAuditRecord, now time.Time) bool {
	if r == nil {
		return false
	}
	id, err := uuid.Parse(r.EventId)
	if err != nil || id == uuid.Nil || !validText(r.SessionId, 256) || !validText(r.Username, 128) || !ip(r.ClientIp) || !ip(r.VpnIp) || r.ObservedMs <= 0 || r.ObservedMs > now.Add(5*time.Minute).UnixMilli() || r.ConnectedMs < 0 || r.ConnectedMs > r.ObservedMs || r.GetBytesIn() > math.MaxInt64 || r.GetBytesOut() > math.MaxInt64 {
		return false
	}
	switch r.EventType {
	case "connect", "disconnect":
		return r.DestinationIp == "" && r.Protocol == "" && r.DestinationPort == nil && r.FlowStartedMs == 0
	case "flow":
		switch r.Protocol {
		case "tcp", "udp", "icmp", "icmpv6", "sctp":
		default:
			return false
		}
		return ip(r.DestinationIp) && r.GetDestinationPort() <= 65535 && r.FlowStartedMs >= r.ConnectedMs && r.FlowStartedMs > 0 && r.FlowStartedMs <= r.ObservedMs
	}
	return false
}

type Store interface {
	CheckAuditFence(context.Context, []byte, []byte, int64) error
	SaveAuditRecord(context.Context, uuid.UUID, *agent.VpnAuditRecord) error
}

func Ingest(ctx context.Context, backend database.Backend, req *wire.StoreVpnAuditRequest) (*agent.VpnAuditReceipt, error) {
	node, err := uuid.FromBytes(req.GetNodeId())
	records := req.GetBatch().GetRecords()
	if err != nil || node == uuid.Nil || len(req.EndpointId) != 32 || len(req.ConnectionId) != 16 || req.SessionEpoch == 0 || req.SessionEpoch > math.MaxInt64 || len(records) == 0 || len(records) > 32 {
		return nil, ErrInvalid
	}
	receipt := &agent.VpnAuditReceipt{}
	now := time.Now()
	for _, r := range records {
		if !valid(r, now) {
			return nil, ErrInvalid
		}
		receipt.EventIds = append(receipt.EventIds, r.EventId)
	}
	err = database.Within(ctx, backend, database.ReadCommitted, func(tx database.Tx) error {
		trust, e := ingress.Ingress(tx).LockTrustShared(ctx, node, req.EndpointId)
		if e != nil {
			return e
		}
		if trust.EndpointState != "active" || (trust.NodeStatus != "active" && trust.NodeStatus != "offline") {
			return ErrInactive
		}
		store := database.Must[Store](tx)
		if e = store.CheckAuditFence(ctx, req.NodeId, req.ConnectionId, int64(req.SessionEpoch)); e != nil {
			return e
		}
		for _, r := range records {
			if e = store.SaveAuditRecord(ctx, node, r); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
