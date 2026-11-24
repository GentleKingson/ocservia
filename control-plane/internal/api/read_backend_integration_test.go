package api

import (
	"context"
	"encoding/json"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/google/uuid"
)

func TestControllerReadsBackendHTTPIntegration(t *testing.T) {
	backend := authenticationBackend(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := backend.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	workspace, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, id := range []uuid.UUID{workspace, other} {
		exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Read API',$2,now(),now())`, id, "read-"+id.String())
	}
	service, err := auth.NewBackend(backend, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	username, password := "read-"+uuid.NewString(), "a long Controller reader test password"
	identity, err := service.CreateLocalCredential(ctx, username, password)
	testutil.NoError(t, err)
	at := time.Now().UTC()
	exec(`INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3)`, workspace, identity, at)
	config := testHTTPConfig(false)
	config.LocalSimulator, config.DevAuthToken = true, "read-backend-development-token-32-characters"
	server := newTestServer(t, config, backend, Modules{TransportEvents: transportevents.NewBackend(backend, nil)}, Authorization{Authentication: service})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	login := authHTTPRequest(server, "POST", "login", `{"username":"`+username+`","password":"`+password+`"}`, authTestOrigin)
	if login.Code != http.StatusNoContent || len(login.Result().Cookies()) != 1 {
		t.Fatalf("reader login: %d %s", login.Code, login.Body)
	}
	cookie := login.Result().Cookies()[0]
	nonAdminName, nonAdminPass := "read-nonadmin-"+uuid.NewString(), "a long Controller reader test password"
	if _, err := service.CreateLocalCredential(ctx, nonAdminName, nonAdminPass); err != nil {
		t.Fatal(err)
	}
	nonAdminLogin := authHTTPRequest(server, "POST", "login", `{"username":"`+nonAdminName+`","password":"`+nonAdminPass+`"}`, authTestOrigin)
	if nonAdminLogin.Code != http.StatusNoContent || len(nonAdminLogin.Result().Cookies()) != 1 {
		t.Fatalf("non-admin reader login: %d %s", nonAdminLogin.Code, nonAdminLogin.Body)
	}
	nonAdminCookie := nonAdminLogin.Result().Cookies()[0]
	get := func(path string, scope uuid.UUID, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1:8080"
		if strings.HasPrefix(path, "/api/v1/development/") {
			r.Header.Set("Authorization", "Bearer "+server.devAuthToken)
		}
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
	get("/api/v1/audit/vpn", other, cookie, http.StatusForbidden)
	get("/api/v1/audit/vpn", workspace, nonAdminCookie, http.StatusForbidden)
	get("/api/v1/audit/vpn?kind=network&destination_ip=bad", workspace, cookie, http.StatusBadRequest)
	for _, kind := range []string{"sessions", "network", "observations"} {
		get("/api/v1/audit/vpn?kind="+kind, workspace, cookie, http.StatusOK)
	}
	// Equal timestamps on the same node must survive pagination without repeating or losing rows.
	auditNode := uuid.New()
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'vpn-reader','offline',now(),now())`, auditNode, workspace)
	for range 3 {
		exec(`INSERT INTO vpn_network_history(node_id,event_id,session_id,username,client_ip,vpn_ip,connected_at,observed_at,destination_ip,destination_port,protocol,flow_started_at,bytes_out)
		 VALUES($1,$2,'42','alice','192.0.2.10','10.0.0.4',$3,$3,'203.0.113.1',443,'tcp',$3,9007199254740993)`, auditNode, uuid.New(), at)
	}
	flowURL := "/api/v1/audit/vpn?kind=network&username=alice&destination_ip=203.0.113.1&page_size=2"
	var flowPage struct {
		Items []struct {
			ID    string `json:"event_id"`
			Bytes string `json:"bytes_out"`
		} `json:"items"`
		Next string `json:"next_cursor"`
	}
	w = get(flowURL, workspace, cookie, http.StatusOK)
	testutil.NoError(t, json.Unmarshal(w.Body.Bytes(), &flowPage))
	if len(flowPage.Items) != 2 || flowPage.Next == "" || flowPage.Items[0].Bytes != "9007199254740993" {
		t.Fatalf("first flow page %s", w.Body)
	}
	first, second := flowPage.Items[0].ID, flowPage.Items[1].ID
	w = get(flowURL+"&cursor="+flowPage.Next, workspace, cookie, http.StatusOK)
	flowPage.Next = ""
	testutil.NoError(t, json.Unmarshal(w.Body.Bytes(), &flowPage))
	if len(flowPage.Items) != 1 || flowPage.Next != "" || flowPage.Items[0].ID == first || flowPage.Items[0].ID == second {
		t.Fatalf("second flow page %s", w.Body)
	}
	get("/api/v1/audit/events", other, cookie, http.StatusForbidden)
	get("/api/v1/audit/events?page_size=201", workspace, cookie, http.StatusBadRequest)

	base := time.Now().UTC().Truncate(time.Microsecond)
	stamps := []time.Time{base.Add(-2 * time.Hour), base.Add(-time.Hour), base}
	var auditIDs []uuid.UUID
	for _, stamp := range stamps {
		id := uuid.Must(uuid.NewV7())
		auditIDs = append(auditIDs, id)
		exec(`INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES($1,$2,$3,'system','fixture','reader.fixture','workspace','read-api','succeeded',$4)`, id, workspace, stamp, make([]byte, 32))
	}
	exec(`INSERT INTO audit_events(id,workspace_id,occurred_at,actor_type,actor_id,action,resource_type,request_id,result,event_hash) VALUES($1,$2,$3,'system','fixture','hidden.fixture','workspace','read-api','succeeded',$4)`, uuid.Must(uuid.NewV7()), other, stamps[2], make([]byte, 32))
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	w = get("/api/v1/audit/events?page_size=3", workspace, cookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 3 {
		t.Fatalf("audit page: %s %v", w.Body, err)
	}
	for i, item := range page.Items {
		var id uuid.UUID
		var stamp time.Time
		if err := json.Unmarshal(item["id"], &id); err != nil || id != auditIDs[2-i] {
			t.Fatalf("audit order/scope: %s %v", item["id"], err)
		}
		if err := json.Unmarshal(item["occurred_at"], &stamp); err != nil || !stamp.Equal(stamps[2-i]) {
			t.Fatalf("audit time: %s %v", item["occurred_at"], err)
		}
		for _, field := range []string{"resource_id", "node_id", "trace_id", "command_id", "reason", "error_type"} {
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
	exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'read node','active',now(),now())`, node, workspace)
	exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,received_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,$2,'read-api',$3,'1.0.0','1.3.0','debian','amd64','{}','{}','{}',$2)`, node, stamps[0], uuid.Must(uuid.NewV7()))
	if err := telemetry.NewBackend(backend).Maintain(ctx); err != nil {
		t.Fatal("offline maintenance", err)
	}
	w = get("/api/v1/events", workspace, cookie, http.StatusOK)
	var events struct {
		Items []transportevents.Event `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil || len(events.Items) != 1 || events.Items[0].NodeID != node.String() || events.Items[0].Type != "disconnected" {
		t.Fatalf("offline event API: %s %v", w.Body, err)
	}
	if events.Items[0].OccurredAt.IsZero() {
		t.Fatal("missing maintenance event time")
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
	get("/readyz", uuid.Nil, nil, http.StatusOK)
	get("/readyz", uuid.Nil, nil, http.StatusOK)
	w = get("/api/v1/workspaces", uuid.Nil, nonAdminCookie, http.StatusOK)
	if err := json.Unmarshal(w.Body.Bytes(), &workspaces); err != nil || len(workspaces.Items) != 0 {
		t.Fatalf("empty authorized workspaces: %s %v", w.Body, err)
	}
	get("/api/v1/audit/events", workspace, nonAdminCookie, http.StatusForbidden)
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
