package nodehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetryread"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

const nodeID = "019fc0a4-6d92-765c-a8a1-4af556614cc3"
const nodePath = "/api/v1/nodes/" + nodeID

type readCall struct {
	method                     string
	workspace, node, after     uuid.UUID
	cursor, metric, resolution string
	limit                      int
	since                      value.Timestamp
}

type readerStub struct {
	calls    []readCall
	observe  func(context.Context)
	err      error
	more     bool
	nodes    []telemetry.Node
	sessions []telemetryread.Session
	bans     []telemetry.IPBan
	history  []telemetry.HistoryPoint
}

func (s *readerStub) record(ctx context.Context, call readCall) {
	s.calls = append(s.calls, call)
	if s.observe != nil {
		s.observe(ctx)
	}
}
func (s *readerStub) ListNodesInWorkspace(ctx context.Context, ws, after uuid.UUID, limit int) ([]telemetry.Node, bool, error) {
	s.record(ctx, readCall{method: "nodes", workspace: ws, after: after, limit: limit})
	return s.nodes, s.more, s.err
}
func (s *readerStub) GetNode(ctx context.Context, id uuid.UUID) (telemetry.Node, error) {
	s.record(ctx, readCall{method: "node", node: id})
	if len(s.nodes) == 0 {
		return telemetry.Node{}, s.err
	}
	return s.nodes[0], s.err
}
func (s *readerStub) ListSessions(ctx context.Context, id uuid.UUID, cursor string, limit int) ([]telemetryread.Session, bool, error) {
	s.record(ctx, readCall{method: "sessions", node: id, cursor: cursor, limit: limit})
	return s.sessions, s.more, s.err
}
func (s *readerStub) ListIPBans(ctx context.Context, id uuid.UUID, limit int) ([]telemetry.IPBan, error) {
	s.record(ctx, readCall{method: "bans", node: id, limit: limit})
	return s.bans, s.err
}
func (s *readerStub) HistoryFrom(ctx context.Context, id uuid.UUID, metric, resolution string, since value.Timestamp) ([]telemetry.HistoryPoint, error) {
	s.record(ctx, readCall{method: "history", node: id, metric: metric, resolution: resolution, since: since})
	return s.history, s.err
}

// Only this unit fixture bypasses authentication. Parent-package tests use
// the real guard and Local sessions.
func testMux(t *testing.T, reader Reader, ws uuid.UUID, log io.Writer) *http.ServeMux {
	t.Helper()
	h := New(slog.New(slog.NewTextHandler(log, nil)), func(*http.Request) uuid.UUID { return ws })
	mux := http.NewServeMux()
	h.Register(mux, func(action string, next http.HandlerFunc) http.HandlerFunc {
		if action != "node.read" {
			t.Fatalf("action = %q", action)
		}
		return next
	})
	h.SetReader(reader) // registration must not capture the initially absent reader
	return mux
}

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func assertJSON(t *testing.T, w *httptest.ResponseRecorder, want any) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != string(encoded)+"\n" {
		t.Fatalf("got %s want %s", w.Body, encoded)
	}
}

func TestNodeReads(t *testing.T) {
	id, ws := uuid.MustParse(nodeID), uuid.Must(uuid.NewV7())
	stamp := value.Timestamp{Valid: true, Micros: value.PositiveInfinity}
	nodes := []telemetry.Node{{ID: nodeID, Name: "configured", RecommendedAgentVersion: "2.0.0", AgentUpgradeEligible: true}}
	sessions := []telemetryread.Session{{ID: "session-last", ConnectedAt: stamp, BytesIn: 123}}
	bans := []telemetry.IPBan{{IP: "2001:db8::9"}}
	history := []telemetry.HistoryPoint{{At: stamp, Metric: "cpu_usage_ratio", Count: 2, Average: 3}}
	for _, tc := range []struct {
		path string
		call readCall
		want any
	}{
		{"/api/v1/nodes", readCall{method: "nodes", workspace: ws, limit: 50}, map[string]any{"items": nodes, "page": map[string]any{"has_more": true, "next_cursor": nodeID}}},
		{"/api/v1/nodes?cursor=" + nodeID + "&page_size=200", readCall{method: "nodes", workspace: ws, after: id, limit: 200}, map[string]any{"items": nodes, "page": map[string]any{"has_more": true, "next_cursor": nodeID}}},
		{nodePath, readCall{method: "node", node: id}, nodes[0]},
		{nodePath + "/sessions", readCall{method: "sessions", node: id, limit: 50}, map[string]any{"items": sessions, "page": map[string]any{"has_more": true, "next_cursor": "session-last"}}},
		{nodePath + "/sessions?cursor=" + strings.Repeat("s", 256) + "&page_size=1", readCall{method: "sessions", node: id, cursor: strings.Repeat("s", 256), limit: 1}, map[string]any{"items": sessions, "page": map[string]any{"has_more": true, "next_cursor": "session-last"}}},
		{nodePath + "/ip-bans?cursor=bad&page_size=0", readCall{method: "bans", node: id, limit: 200}, map[string]any{"items": bans}},
		{nodePath + "/telemetry?metric=cpu_usage_ratio", readCall{method: "history", node: id, metric: "cpu_usage_ratio", resolution: "5m"}, map[string]any{"items": history, "resolution": "5m"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			reader := &readerStub{nodes: nodes, sessions: sessions, bans: bans, history: history, more: true}
			assertJSON(t, get(testMux(t, reader, ws, io.Discard), tc.path), tc.want)
			if !reflect.DeepEqual(reader.calls, []readCall{tc.call}) {
				t.Fatalf("calls: %#v want %#v", reader.calls, tc.call)
			}
		})
	}
	for _, more := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			reader := &readerStub{nodes: nodes, sessions: sessions, more: more}
			if empty {
				reader.nodes = []telemetry.Node{}
				reader.sessions = []telemetryread.Session{}
			}
			mux := testMux(t, reader, ws, io.Discard)
			for _, tc := range []struct {
				path, cursor string
				items        any
			}{
				{"/api/v1/nodes", nodeID, reader.nodes}, {nodePath + "/sessions", "session-last", reader.sessions},
			} {
				page := map[string]any{"has_more": more}
				if more && !empty {
					page["next_cursor"] = tc.cursor
				}
				assertJSON(t, get(mux, tc.path), map[string]any{"items": tc.items, "page": page})
			}
		}
	}
}

func TestNodeReadParsing(t *testing.T) {
	for _, tc := range []struct{ path, detail string }{
		{"/api/v1/nodes?cursor=bad&page_size=0", "cursor must be a UUIDv7 node ID"},
		{"/api/v1/nodes?cursor=519fc0a4-6d92-465c-a8a1-4af556614cc3", "cursor must be a UUIDv7 node ID"},
		{"/api/v1/nodes?page_size=0", "page_size must be between 1 and 200"},
		{"/api/v1/nodes?page_size=201", "page_size must be between 1 and 200"},
		{"/api/v1/nodes?page_size=abc", "page_size must be between 1 and 200"},
		{nodePath + "/sessions?page_size=201", "cursor and page_size are outside permitted bounds"},
		{nodePath + "/sessions?cursor=" + strings.Repeat("s", 257), "cursor and page_size are outside permitted bounds"},
		{nodePath + "/telemetry?since=bad", "since must be an RFC 3339 timestamp, signed six-digit extended-year timestamp, infinity or -infinity"},
	} {
		reader := &readerStub{}
		w := get(testMux(t, reader, uuid.Nil, io.Discard), tc.path)
		if w.Code != 400 || !strings.Contains(w.Body.String(), tc.detail) || len(reader.calls) != 0 {
			t.Fatalf("%s: %d %s calls=%v", tc.path, w.Code, w.Body, reader.calls)
		}
	}
	for _, id := range []string{"bad", "519fc0a4-6d92-465c-a8a1-4af556614cc3"} {
		for _, suffix := range []string{"", "/sessions", "/ip-bans", "/telemetry"} {
			reader := &readerStub{}
			w := get(testMux(t, reader, uuid.Nil, io.Discard), "/api/v1/nodes/"+id+suffix)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "node_id must be a UUIDv7") || len(reader.calls) != 0 {
				t.Fatalf("invalid ID: %d %s", w.Code, w.Body)
			}
		}
	}
	for _, text := range []string{"", "infinity", "-infinity", "+010000-01-01T00:00:00Z", "-000001-01-01T00:00:00Z", "2026-01-02T03:04:05.123456Z"} {
		want := value.Timestamp{}
		if text != "" {
			var err error
			want, err = value.ParseTimestamp(text)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, resolution := range []string{"raw", "1h"} {
			reader := &readerStub{history: []telemetry.HistoryPoint{}}
			w := get(testMux(t, reader, uuid.Nil, io.Discard), nodePath+"/telemetry?metric=cpu_usage_ratio&resolution="+resolution+"&since="+url.QueryEscape(text))
			assertJSON(t, w, map[string]any{"items": []telemetry.HistoryPoint{}, "resolution": resolution})
			wantCall := readCall{method: "history", node: uuid.MustParse(nodeID), metric: "cpu_usage_ratio", resolution: resolution, since: want}
			if !reflect.DeepEqual(reader.calls, []readCall{wantCall}) {
				t.Fatalf("time arguments: %+v", reader.calls)
			}
		}
	}
}

func TestNodeReadErrors(t *testing.T) {
	internal := errors.New("private diagnostic")
	for _, tc := range []struct {
		path, kind, title, detail string
		err                       error
		status                    int
	}{
		{"/api/v1/nodes", "database-unavailable", "Nodes unavailable", "node state could not be read", internal, 503},
		{nodePath, "database-unavailable", "Node unavailable", "node state could not be read", internal, 503},
		{nodePath, "not-found", "Node not found", "the requested node does not exist", fmt.Errorf("wrapped: %w", database.ErrNotFound), 404},
		{nodePath + "/sessions", "database-unavailable", "Sessions unavailable", "session state could not be read", internal, 503},
		{nodePath + "/ip-bans", "database-unavailable", "IP bans unavailable", "IP ban state could not be read", internal, 503},
		{nodePath + "/telemetry", "database-unavailable", "Telemetry unavailable", "telemetry history could not be read", internal, 503},
		{nodePath + "/telemetry", "invalid-query", "Invalid query", "metric is invalid", fmt.Errorf("wrapped: %w", telemetry.ErrInvalidMetric), 400},
		{nodePath + "/telemetry", "invalid-query", "Invalid query", "resolution is invalid", fmt.Errorf("wrapped: %w", telemetry.ErrInvalidResolution), 400},
	} {
		var logs bytes.Buffer
		reader := &readerStub{err: tc.err}
		mux := testMux(t, reader, uuid.Nil, &logs)
		r := httptest.NewRequest("GET", tc.path, nil)
		tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
		r = r.WithContext(trace.ContextWithSpanContext(r.Context(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid})))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"type": "https://ocservia.dev/problems/" + tc.kind, "title": tc.title, "status": float64(tc.status), "detail": tc.detail, "instance": tc.path, "trace_id": tid.String()}
		if w.Code != tc.status || w.Header().Get("Content-Type") != "application/problem+json" || !reflect.DeepEqual(got, want) || len(reader.calls) != 1 {
			t.Fatalf("problem: %d %s calls=%v", w.Code, w.Body, reader.calls)
		}
		if tc.err == internal && (tc.path == "/api/v1/nodes" || tc.path == nodePath+"/telemetry") && !strings.Contains(logs.String(), internal.Error()) {
			t.Fatal("missing diagnostic")
		}
	}
}

func TestNodeReadUnavailable(t *testing.T) {
	mux := testMux(t, nil, uuid.Nil, io.Discard)
	for _, path := range []string{"/api/v1/nodes?cursor=bad", "/api/v1/nodes/bad", "/api/v1/nodes/bad/sessions", "/api/v1/nodes/bad/ip-bans", "/api/v1/nodes/bad/telemetry"} {
		w := get(mux, path)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "the telemetry read model is unavailable") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestNodeReadContext(t *testing.T) {
	for _, path := range []string{"/api/v1/nodes", nodePath, nodePath + "/sessions", nodePath + "/ip-bans", nodePath + "/telemetry"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			reader := &readerStub{observe: func(got context.Context) {
				if got != ctx {
					t.Error("request context replaced")
				}
				if at, ok := got.Deadline(); !ok || at != deadline {
					t.Error("deadline lost")
				}
				cancel()
				if got.Err() != context.Canceled {
					t.Error("cancellation lost")
				}
			}}
			w := httptest.NewRecorder()
			testMux(t, reader, uuid.Nil, io.Discard).ServeHTTP(w, httptest.NewRequest("GET", path, nil).WithContext(ctx))
			if len(reader.calls) != 1 {
				t.Fatal("reader not called")
			}
		})
	}
}

func TestNodeRoutesRequireGuard(t *testing.T) {
	h := New(slog.Default(), func(*http.Request) uuid.UUID { return uuid.Nil })
	t.Run("missing", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("missing guard accepted")
			}
		}()
		h.Register(http.NewServeMux(), nil)
	})
	called := 0
	mux := http.NewServeMux()
	h.Register(mux, func(action string, next http.HandlerFunc) http.HandlerFunc {
		if action != "node.read" {
			t.Fatalf("action = %q", action)
		}
		return func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(401) }
	})
	for _, path := range []string{"/api/v1/nodes", nodePath, nodePath + "/sessions", nodePath + "/ip-bans", nodePath + "/telemetry"} {
		if w := get(mux, path); w.Code != 401 {
			t.Fatalf("guard bypassed: %d", w.Code)
		}
	}
	if called != 5 {
		t.Fatalf("guard calls = %d", called)
	}
	for _, path := range []string{nodePath + "/sessions/42:disconnect", nodePath + "/ip-bans/192.0.2.1:remove", nodePath + "/user-group-state"} {
		if w := get(mux, path); w.Code != 404 {
			t.Fatalf("module claimed adjacent path: %s %d", path, w.Code)
		}
	}
}
