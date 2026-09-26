package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

// Called inside the existing isolated Controller startup database, never the
// PostgreSQL database used by later Bootstrap acceptance.
func testHTTPServerAssembly(t *testing.T, cfg config.Config, owner, runtime *connection.Connection) {
	cfg.HTTPAddress, cfg.PublicOrigin = e2eAddress(t), "https://assembly.example.test"
	cfg.LocalAuth, cfg.DevAuth = true, false
	cfg.SessionKey, cfg.SessionTTL = make([]byte, 32), time.Hour
	cfg.RecommendedAgentVersion = "2.0.0"
	cfg.EventStreams.IdentityStreams, cfg.EventStreams.SessionStreams = 1, 1
	cfg.EventStreams.RetryAfter, cfg.EventStreams.RevalidateInterval = 7*time.Second, 5*time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	seedAuth, err := auth.NewBackend(runtime.Store, auth.Config{LocalEnabled: true, SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL})
	if err != nil {
		t.Fatal(err)
	}
	username, password := "s01-"+uuid.NewString(), "unique long HTTP assembly fixture password"
	identity, err := seedAuth.CreateLocalCredential(ctx, username, password)
	if err != nil {
		t.Fatal(err)
	}
	workspace, node, binding := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	at, _ := value.FromTime(time.Now().UTC())
	exec := func(pg, my string, args ...any) {
		t.Helper()
		query := pg
		if cfg.DatabaseBackend != "postgres" {
			query = my
			for i, arg := range args {
				if id, ok := arg.(uuid.UUID); ok {
					args[i] = mysql.UUIDBytes(id)
				}
			}
		}
		if _, err := owner.Store.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'S01',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'S01',?,?,?)`, workspace, workspace.String(), at, at)
	exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'S01','active',1,$3,$4)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,'S01','active',1,?,?)`, node, workspace, at, at)
	exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'PlatformAdmin','workspace',?)`, binding, identity, workspace, at)
	signer := commandauth.NewSignerFromSeed([32]byte{1})
	ops := operations.NewBackend(runtime.Store, 3, signer)
	users := userstate.NewWithSignerBackend(runtime.Store, signer)
	services := httpServices{
		modules:    api.Modules{Nodes: telemetry.NewWithRecommendedAgentVersionBackend(runtime.Store, cfg.RecommendedAgentVersion), ConfigPlans: configplan.NewBackend(runtime.Store, ops), UserOperations: useroperations.NewWithConcurrencyBackend(runtime.Store, users, 3)},
		operations: ops, userState: users, localSlice: localslice.NewBackend(runtime.Store, signer),
	}
	life := newLifecycle(ctx, time.Second, quietLogger())
	t.Cleanup(func() {
		if err := life.close(nil); err != nil {
			t.Error(err)
		}
	})
	server, err := newHTTPServer(life, cfg, BuildInfo{Version: "s01"}, runtime.Store, nil, quietLogger(), services)
	if err != nil || life.http != server {
		t.Fatal("production assembly ownership", err)
	}
	// Mutation of the assembly inputs cannot disable a returned Server.
	services.modules = api.Modules{}
	cfg.EventStreams.IdentityStreams = 8
	life.start("serve HTTP", func(context.Context) error { return server.ListenAndServe() })
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	base := "http://" + cfg.HTTPAddress
	for {
		response, err := client.Get(base + "/livez")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		select {
		case task := <-life.first:
			t.Fatalf("HTTP startup: %v", task.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	request, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", cfg.PublicOrigin)
	login, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != 204 || len(login.Cookies()) != 1 {
		t.Fatalf("real Local login: %d", login.StatusCode)
	}
	cookie := login.Cookies()[0]
	call := func(method, path string) *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, method, base+path, nil)
		r.AddCookie(cookie)
		r.Header.Set("Origin", cfg.PublicOrigin)
		r.Header.Set("X-Workspace-ID", workspace.String())
		w, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, path := range []string{"/api/v1/nodes/" + node.String(), "/api/v1/user-operations/metrics"} {
		response := call("GET", path)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("first %s: %d %s %v", path, response.StatusCode, body, err)
		}
		if strings.Contains(path, "/nodes/") {
			var got telemetry.Node
			if json.Unmarshal(body, &got) != nil || got.RecommendedAgentVersion != "2.0.0" {
				t.Fatalf("configured Reader lost: %s", body)
			}
		}
	}
	plan := call("POST", "/api/v1/nodes/"+node.String()+"/config-plans")
	body, _ := io.ReadAll(plan.Body)
	plan.Body.Close()
	if plan.StatusCode != 400 || !strings.Contains(string(body), "idempotency-key-required") {
		t.Fatalf("first ConfigPlan request did not reach configured module: %d %s", plan.StatusCode, body)
	}
	stream := call("GET", "/api/v1/events/stream")
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("first stream: %d", stream.StatusCode)
	}
	rejected := call("GET", "/api/v1/events/stream")
	rejected.Body.Close()
	if rejected.StatusCode != 429 || rejected.Header.Get("Retry-After") != "7" || rejected.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("final SSE configuration not used: %d %v", rejected.StatusCode, rejected.Header)
	}
	exec(`DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, binding)
	denied := call("GET", "/api/v1/nodes/"+node.String())
	denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatal("permissions were frozen at construction", denied.StatusCode)
	}
	// Wait for the bounded revalidation signal (EOF), not a fixed sleep.
	if _, err := io.ReadAll(stream.Body); err != nil {
		t.Fatal("revoked SSE did not close on revalidation", err)
	}
}
