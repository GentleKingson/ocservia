package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry/store"
	"github.com/google/uuid"
)

type observedNodeReader struct {
	NodeReader
	calls atomic.Int64
}

func (r *observedNodeReader) ListNodesInWorkspace(ctx context.Context, ws, after uuid.UUID, limit int) ([]telemetry.Node, bool, error) {
	r.calls.Add(1)
	return r.NodeReader.ListNodesInWorkspace(ctx, ws, after, limit)
}
func (r *observedNodeReader) GetNode(ctx context.Context, id uuid.UUID) (telemetry.Node, error) {
	r.calls.Add(1)
	return r.NodeReader.GetNode(ctx, id)
}
func (r *observedNodeReader) ListSessions(ctx context.Context, id uuid.UUID, cursor string, limit int) ([]telemetrystore.ReadSession, bool, error) {
	r.calls.Add(1)
	return r.NodeReader.ListSessions(ctx, id, cursor, limit)
}
func (r *observedNodeReader) ListIPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetry.IPBan, error) {
	r.calls.Add(1)
	return r.NodeReader.ListIPBans(ctx, id, limit)
}
func (r *observedNodeReader) HistoryFrom(ctx context.Context, id uuid.UUID, metric, resolution string, since value.Timestamp) ([]telemetry.HistoryPoint, error) {
	r.calls.Add(1)
	return r.NodeReader.HistoryFrom(ctx, id, metric, resolution, since)
}

type blockedNodeReader struct {
	NodeReader
	*blockedRequest
}

func (r *blockedNodeReader) GetNode(ctx context.Context, _ uuid.UUID) (telemetry.Node, error) {
	return telemetry.Node{}, r.wait(ctx)
}

func TestNodeHTTPContextLifetime(t *testing.T) {
	checkRequestLifetime(t, "", func() *http.Request { return baselineRequest("GET", "/api/v1/nodes/"+baselineID, nil) }, func(b *blockedRequest) Modules {
		return Modules{Nodes: &blockedNodeReader{blockedRequest: b}}
	}, Authorization{})
}
