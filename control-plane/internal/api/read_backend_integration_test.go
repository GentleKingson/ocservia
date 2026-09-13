package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
)

func TestControllerReadsBackendHTTPIntegration(t *testing.T) {
	backend := authenticationBackend(t)
	ctx := context.Background()
	_, mysqlEngine := backend.(*mysql.Backend)
	exec := func(pg, my string, args ...any) {
		t.Helper()
		if mysqlEngine {
			pg = my
			for i, arg := range args {
				if id, ok := arg.(uuid.UUID); ok {
					args[i] = mysql.UUIDBytes(id)
				}
			}
		}
		if _, err := backend.Exec(ctx, pg, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, id := range []uuid.UUID{workspace, other} {
		exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Read API',$2,now(),now())`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Read API',?,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, id, "read-"+id.String())
	}
	service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	username, password := "read-"+uuid.NewString(), "a long Controller reader test password"
	identity, err := service.CreateLocalCredential(ctx, username, password)
	if err != nil {
		t.Fatal(err)
	}
	at, _ := value.FromTime(time.Now().UTC())
	bindingID := uuid.Must(uuid.NewV7())
	exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'PlatformAdmin','workspace',?)`, bindingID, identity, workspace, at)
	server := NewBackend("127.0.0.1:0", backend, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, 15*time.Second, false, "", 35)
	server.EnableAuthorization(service, rbac.NewBackend(backend), nil, nil)
	server.EnableBrowserOrigin(authTestOrigin)
	server.EnableLocalSlice(localslice.NewBackend(backend, nil))
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	login := authHTTPRequest(server, "POST", "login", `{"username":"`+username+`","password":"`+password+`"}`, authTestOrigin)
	if login.Code != http.StatusNoContent || len(login.Result().Cookies()) != 1 {
		t.Fatalf("reader login: %d %s", login.Code, login.Body)
	}
	cookie := login.Result().Cookies()[0]
	get := func(path string, scope uuid.UUID, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if scope != uuid.Nil {
			r.Header.Set("X-Workspace-ID", scope.String())
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("GET %s: %d want %d: %s", path, w.Code, want, w.Body)
		}
		return w
	}
	var workspaces struct {
		Items []struct{ ID uuid.UUID } `json:"items"`
	}
	w := get("/api/v1/workspaces", uuid.Nil, cookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &workspaces); err != nil || len(workspaces.Items) != 1 || workspaces.Items[0].ID != workspace {
		t.Fatalf("workspace isolation: %s %v", w.Body, err)
	}
	get("/api/v1/workspaces", uuid.Nil, nil, http.StatusUnauthorized)
	get("/api/v1/audit/events", other, cookie, http.StatusForbidden)
	get("/api/v1/audit/events?page_size=201", workspace, cookie, http.StatusBadRequest)

	// Legacy rows deliberately exercise the reader's complete timestamp domain,
	// not authenticated audit-chain creation or verification.
	stamps := []value.Timestamp{{Micros: value.NegativeInfinity, Valid: true}, {Micros: value.EndTimestamp - 1, Valid: true}, {Micros: value.PositiveInfinity, Valid: true}}
	var auditIDs []uuid.UUID
	for _, stamp := range stamps {
		id := uuid.Must(uuid.NewV7())
		auditIDs = append(auditIDs, id)
		exec(`INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES($1,$2,$3,'system','fixture','reader.fixture','workspace','read-api','succeeded',$4)`, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES(?,?,?,'system','fixture','reader.fixture','workspace','read-api','succeeded',?)`, id, workspace, stamp, make([]byte, 32))
	}
	exec(`INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES($1,$2,$3,'system','fixture','hidden.fixture','workspace','read-api','succeeded',$4)`, `INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES(?,?,?,'system','fixture','hidden.fixture','workspace','read-api','succeeded',?)`, uuid.Must(uuid.NewV7()), other, stamps[2], make([]byte, 32))
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	w = get("/api/v1/audit/events?page_size=3", workspace, cookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 3 {
		t.Fatalf("audit page: %s %v", w.Body, err)
	}
	for i, item := range page.Items {
		var id uuid.UUID
		var stamp value.Timestamp
		if err := json.Unmarshal(item["id"], &id); err != nil || id != auditIDs[2-i] {
			t.Fatalf("audit order/scope: %s %v", item["id"], err)
		}
		if err := json.Unmarshal(item["occurred_at"], &stamp); err != nil || stamp != stamps[2-i] {
			t.Fatalf("audit time: %s %v", item["occurred_at"], err)
		}
		for _, field := range []string{"resource_id", "node_id", "trace_id", "command_id", "approval_id", "reason", "error_type"} {
			if string(item[field]) != "null" {
				t.Fatalf("nullable %s changed: %s", field, item[field])
			}
		}
	}
	w = get("/api/v1/audit/events?page_size=1", workspace, cookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("audit limit: %s %v", w.Body, err)
	}

	node := uuid.Must(uuid.NewV7())
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'read node','active',now(),now())`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'read node','active',TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)),TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)))`, node, workspace)
	if scope, architecture, version, err := server.upgradeNode(ctx, node); err != nil || scope != workspace || architecture != "" || version != "" {
		t.Fatalf("unobserved upgrade target: %s %q %q %v", scope, architecture, version, err)
	}
	if _, _, _, err := server.upgradeNode(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("missing upgrade target: %v", err)
	}
	exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,$2,'read-api',$3,'1.0.0','1.3.0','debian','amd64','{}','{}','{}',$2)`, "INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,`system`,path,last_heartbeat_at) VALUES(?,? ,?,'read-api',?,'1.0.0','1.3.0','debian','amd64','{}','{}','{}',?)", readSnapshotArgs(mysqlEngine, node, stamps[0])...)
	if scope, architecture, version, err := server.upgradeNode(ctx, node); err != nil || scope != workspace || architecture != "amd64" || version != "1.0.0" {
		t.Fatalf("observed upgrade target: %s %q %q %v", scope, architecture, version, err)
	}
	if err := telemetry.NewBackend(backend).Maintain(ctx); err != nil {
		t.Fatal("offline maintenance", err)
	}
	w = get("/api/v1/events", workspace, cookie, http.StatusOK)
	var events struct {
		Items []localslice.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil || len(events.Items) != 1 || events.Items[0].NodeID != node.String() || events.Items[0].Type != "disconnected" {
		t.Fatalf("offline event API: %s %v", w.Body, err)
	}
	if _, err := events.Items[0].OccurredAt.Time(); err != nil {
		t.Fatal("non-finite maintenance event", err)
	}
	w = get("/api/v1/development/runtime", uuid.Nil, nil, http.StatusOK)
	var metrics struct {
		Available bool                     `json:"privd_attestation_key_states_available"`
		Keys      []struct{ State string } `json:"privd_attestation_key_states"`
		Acquired  int64                    `json:"db_acquired"`
		Idle      int64                    `json:"db_idle"`
		Total     int64                    `json:"db_total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &metrics); err != nil || !metrics.Available || len(metrics.Keys) != 3 || metrics.Keys[0].State != "pending" || metrics.Keys[1].State != "active" || metrics.Keys[2].State != "revoked" {
		t.Fatalf("runtime metrics: %s %v", w.Body, err)
	}
	if metrics.Total < 1 || metrics.Acquired < 0 || metrics.Idle < 0 || metrics.Total != metrics.Acquired+metrics.Idle {
		t.Fatalf("pool metrics: %+v", metrics)
	}
	w = get("/readyz", uuid.Nil, nil, http.StatusOK)
	var ready struct {
		Schema int64 `json:"schema_version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ready); err != nil || ready.Schema != 35 {
		t.Fatalf("schema readiness: %s %v", w.Body, err)
	}
	server.expectedSchema = 36
	get("/readyz", uuid.Nil, nil, http.StatusServiceUnavailable)
	server.expectedSchema = 35
	get("/readyz", uuid.Nil, nil, http.StatusOK)
	exec(`DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, bindingID)
	w = get("/api/v1/workspaces", uuid.Nil, cookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &workspaces); err != nil || len(workspaces.Items) != 0 {
		t.Fatalf("empty authorized workspaces: %s %v", w.Body, err)
	}
	get("/api/v1/audit/events", workspace, cookie, http.StatusForbidden)
	server.devAuth = true
	w = get("/api/v1/workspaces", uuid.Nil, nil, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &workspaces); err != nil {
		t.Fatal(err)
	}
	visible := map[uuid.UUID]bool{}
	for _, item := range workspaces.Items {
		visible[item.ID] = true
	}
	if !visible[workspace] || !visible[other] {
		t.Fatalf("development workspaces: %s", w.Body)
	}
	server.devAuth = false
	server.backend = nil
	get("/readyz", uuid.Nil, nil, http.StatusServiceUnavailable)
	w = get("/api/v1/development/runtime", uuid.Nil, nil, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &metrics); err != nil || metrics.Available || len(metrics.Keys) != 3 {
		t.Fatalf("unavailable metrics: %s %v", w.Body, err)
	}
}

func readSnapshotArgs(mysqlEngine bool, node uuid.UUID, stamp value.Timestamp) []any {
	instance := uuid.Must(uuid.NewV7())
	if mysqlEngine {
		return []any{node, stamp, stamp, instance, stamp}
	}
	return []any{node, stamp, instance}
}
