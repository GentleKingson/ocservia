package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

func assertBaselineJSON(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Request-ID") != "baseline-request" {
		t.Fatalf("JSON response: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(w.Body.Bytes(), &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s\nwant %s", w.Body, want)
	}
}

func TestNodeReadsBackendHTTPBaseline(t *testing.T) {
	b := authenticationBackend(t)
	ctx := context.Background()
	exec := func(pg, my string, args ...any) { t.Helper(); authSafetyExec(t, b, pg, my, args...) }
	ws, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	stamp := value.Timestamp{Valid: true, Micros: 0}
	for _, id := range []uuid.UUID{ws, other} {
		exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'HTTP baseline',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'HTTP baseline',?,?,?)`, id, "baseline-"+id.String(), stamp, stamp)
	}
	authn, err := auth.NewBackend(b, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	username, password := "baseline-"+uuid.NewString(), "a unique long HTTP baseline password"
	identity, err := authn.CreateLocalCredential(ctx, username, password)
	if err != nil {
		t.Fatal(err)
	}
	binding := uuid.Must(uuid.NewV7())
	exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'PlatformAdmin','workspace',?)`, binding, identity, ws, stamp)
	s := baselineServer(t, false)
	s.backend = b
	s.EnableAuthorization(authn, rbac.NewBackend(b), nil, nil)
	s.EnableTelemetry(telemetry.NewBackend(b))
	s.EnableBrowserOrigin(authTestOrigin)
	login := authHTTPRequest(s, "POST", "login", fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), authTestOrigin)
	if login.Code != 204 || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookie := login.Result().Cookies()[0]
	get := func(path string, scope uuid.UUID, session *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := baselineRequest("GET", path, nil)
		if scope != uuid.Nil {
			r.Header.Set("X-Workspace-ID", scope.String())
		}
		if session != nil {
			r.AddCookie(session)
		}
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		return w
	}
	// Enough rows to lock the default page size as well as cursor ordering.
	var ids []uuid.UUID
	for i := 0; i < 52; i++ {
		id, scope := uuid.Must(uuid.NewV7()), ws
		if i == 51 {
			scope = other
		}
		ids = append(ids, id)
		exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',7,$4,$5)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,?,'active',7,?,?)`, id, scope, "node-"+id.String(), stamp, stamp)
	}
	nodePath := "/api/v1/nodes/" + ids[0].String()
	nodeJSON := func(id uuid.UUID, sessions int) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"version":7,"trust_status":"active","connection_state":"offline","freshness":"never","agent_version_state":"unknown","agent_upgrade_eligible":false,"dropped":{"security":0,"health":0,"aggregate":0,"raw":0},"session_count":%d}`, id, "node-"+id.String(), sessions)
	}
	assertBaselineJSON(t, get(nodePath, uuid.Nil, cookie), nodeJSON(ids[0], 0))
	// Resource ownership, not a caller-supplied workspace header, selects a node's scope.
	assertBaselineJSON(t, get(nodePath, other, cookie), nodeJSON(ids[0], 0))
	var first50 []string
	for _, id := range ids[:50] {
		first50 = append(first50, nodeJSON(id, 0))
	}
	assertBaselineJSON(t, get("/api/v1/nodes", uuid.Nil, cookie), fmt.Sprintf(`{"items":[%s],"page":{"has_more":true,"next_cursor":%q}}`, strings.Join(first50, ","), ids[49]))
	assertBaselineJSON(t, get("/api/v1/nodes?page_size=1", ws, cookie), fmt.Sprintf(`{"items":[%s],"page":{"has_more":true,"next_cursor":%q}}`, nodeJSON(ids[0], 0), ids[0]))
	assertBaselineJSON(t, get("/api/v1/nodes?cursor="+ids[49].String(), ws, cookie), `{"items":[`+nodeJSON(ids[50], 0)+`],"page":{"has_more":false}}`)
	assertBaselineJSON(t, get("/api/v1/nodes?cursor="+ids[50].String(), ws, cookie), `{"items":[],"page":{"has_more":false}}`)
	assertBaselineJSON(t, get(nodePath+"/sessions", ws, cookie), `{"items":[],"page":{"has_more":false}}`)
	assertBaselineJSON(t, get(nodePath+"/ip-bans", ws, cookie), `{"items":[]}`)
	for i := 0; i < 51; i++ {
		exec(`INSERT INTO node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES($1,$2,'alice','2001:db8::10',$3,123,456,$4)`, `INSERT INTO node_sessions(node_id,session_id,username,client_ip,connected_at,bytes_in,bytes_out,observed_at) VALUES(?,?,'alice','2001:db8::10',?,123,456,?)`, ids[0], fmt.Sprintf("s%02d", i), stamp, stamp)
	}
	sessionJSON := func(i int) string {
		return fmt.Sprintf(`{"id":"s%02d","username":"alice","client_ip":"2001:db8::10","connected_at":"2000-01-01T00:00:00Z","bytes_in":123,"bytes_out":456}`, i)
	}
	first50 = nil
	for i := 0; i < 50; i++ {
		first50 = append(first50, sessionJSON(i))
	}
	assertBaselineJSON(t, get(nodePath+"/sessions", ws, cookie), `{"items":[`+strings.Join(first50, ",")+`],"page":{"has_more":true,"next_cursor":"s49"}}`)
	assertBaselineJSON(t, get(nodePath+"/sessions?page_size=1", ws, cookie), `{"items":[`+sessionJSON(0)+`],"page":{"has_more":true,"next_cursor":"s00"}}`)
	assertBaselineJSON(t, get(nodePath+"/sessions?cursor=s49", ws, cookie), `{"items":[`+sessionJSON(50)+`],"page":{"has_more":false}}`)
	assertBaselineJSON(t, get(nodePath+"/sessions?cursor=zzz", ws, cookie), `{"items":[],"page":{"has_more":false}}`)
	assertBaselineJSON(t, get(nodePath, ws, cookie), nodeJSON(ids[0], 51))
	exec(`INSERT INTO node_ip_bans(node_id,ip,seconds_remaining,observed_at) VALUES($1,'192.0.2.9',20,$2),($3,'2001:db8::20',NULL,$4)`, `INSERT INTO node_ip_bans(node_id,ip,seconds_remaining,observed_at) VALUES(?,'192.0.2.9',20,?),(?,'2001:db8::20',NULL,?)`, ids[0], stamp, ids[0], stamp)
	for _, query := range []string{"", "?cursor=invalid&page_size=0"} {
		assertBaselineJSON(t, get(nodePath+"/ip-bans"+query, ws, cookie), `{"items":[{"ip":"192.0.2.9","seconds_remaining":20},{"ip":"2001:db8::20"}]}`)
	}
	// A rollup needs no raw-history partition and keeps time independent of the
	// wall clock. Omitted since excludes this old point; -infinity includes it.
	exec(`INSERT INTO telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES($1,'cpu_usage_ratio',$2,2,1,3,2)`, `INSERT INTO telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) VALUES(?,'cpu_usage_ratio',?,2,1,3,2)`, ids[0], stamp)
	assertBaselineJSON(t, get(nodePath+"/telemetry?metric=cpu_usage_ratio", ws, cookie), `{"items":[],"resolution":"5m"}`)
	assertBaselineJSON(t, get(nodePath+"/telemetry?metric=cpu_usage_ratio&since=-infinity", ws, cookie), `{"items":[{"at":"2000-01-01T00:00:00Z","metric":"cpu_usage_ratio","count":2,"minimum":1,"maximum":3,"average":2}],"resolution":"5m"}`)
	for _, resolution := range []string{"raw", "1h"} {
		assertBaselineJSON(t, get(nodePath+"/telemetry?metric=cpu_usage_ratio&resolution="+resolution+"&since=infinity", ws, cookie), `{"items":[],"resolution":"`+resolution+`"}`)
	}
	for _, tc := range []struct{ path, kind, title, detail string }{
		{"/api/v1/nodes?cursor=bad", "invalid-cursor", "Invalid cursor", "cursor must be a UUIDv7 node ID"},
		{"/api/v1/nodes?page_size=201", "invalid-page-size", "Invalid page size", "page_size must be between 1 and 200"},
		{"/api/v1/nodes?page_size=0", "invalid-page-size", "Invalid page size", "page_size must be between 1 and 200"},
		{nodePath + "/sessions?page_size=abc", "invalid-pagination", "Invalid pagination", "cursor and page_size are outside permitted bounds"},
		{nodePath + "/sessions?cursor=" + strings.Repeat("a", 257), "invalid-pagination", "Invalid pagination", "cursor and page_size are outside permitted bounds"},
		{nodePath + "/telemetry", "invalid-query", "Invalid query", "metric is invalid"},
		{nodePath + "/telemetry?metric=cpu_usage_ratio&resolution=bad", "invalid-query", "Invalid query", "resolution is invalid"},
		{nodePath + "/telemetry?metric=bad&since=bad", "invalid-query", "Invalid query", "since must be an RFC 3339 timestamp, signed six-digit extended-year timestamp, infinity or -infinity"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			path, _, _ := strings.Cut(tc.path, "?")
			assertBaselineProblem(t, get(tc.path, ws, cookie), path, 400, tc.kind, tc.title, tc.detail)
		})
	}
	for _, suffix := range []string{"", "/sessions", "/ip-bans", "/telemetry?metric=cpu_usage_ratio"} {
		path, _, _ := strings.Cut(nodePath+suffix, "?")
		assertBaselineProblem(t, get(nodePath+suffix, ws, nil), path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
		for _, id := range []string{"invalid", uuid.Must(uuid.NewV7()).String()} {
			missing := "/api/v1/nodes/" + id + suffix
			path, _, _ := strings.Cut(missing, "?")
			assertBaselineProblem(t, get(missing, ws, cookie), path, 404, "not-found", "Resource not found", "the requested resource does not exist")
		}
		foreign := "/api/v1/nodes/" + ids[51].String() + suffix
		foreignPath, _, _ := strings.Cut(foreign, "?")
		assertBaselineProblem(t, get(foreign, ws, cookie), foreignPath, 403, "forbidden", "Access denied", "the principal is not authorized for this resource and action")
	}
	assertBaselineProblem(t, get("/api/v1/nodes", other, cookie), "/api/v1/nodes", 403, "forbidden", "Access denied", "the principal is not authorized for this resource and action")
	exec(`DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, binding)
	for _, path := range []string{"/api/v1/nodes", nodePath, nodePath + "/sessions", nodePath + "/ip-bans", nodePath + "/telemetry"} {
		assertBaselineProblem(t, get(path, ws, cookie), path, 403, "forbidden", "Access denied", "the principal is not authorized for this resource and action")
	}
}
