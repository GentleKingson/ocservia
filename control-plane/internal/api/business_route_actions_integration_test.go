package api

import (
	"encoding/json"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

func testBusinessRouteActionsBackendHTTPIntegration(t *testing.T, f applyHTTPFixture) {
	// Reuse the restricted-backend, real Local login and HTTP Plan fixture.
	plan := f.plan(false)
	plans := &observedConfigPlans{ConfigPlans: f.s.configPlans.(*configplan.Service)}
	userOperations := useroperations.NewBackend(f.b, nil)
	f.s = f.newServer(Modules{ConfigPlans: plans, UserOperations: userOperations, Certificates: f.s.certificates})
	other, sibling, foreign := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	stamp := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Business routes',$2,$3,$4)`, other, other.String(), stamp, stamp)
	for _, node := range []struct{ id, workspace uuid.UUID }{{sibling, f.workspace}, {foreign, other}} {
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$5)`, node.id, node.workspace, node.id.String(), stamp, stamp)
	}
	for _, node := range []uuid.UUID{plan.NodeID, sibling, foreign} {
		f.exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,created_at,updated_at) VALUES($1,'alice',true,1,1,$2,$3)`, node, stamp, stamp)
	}
	nodePath := "/api/v1/nodes/" + plan.NodeID.String()
	policyPath, planPath := nodePath+"/users/alice/policy", "/api/v1/config-plans/"+plan.ID.String()
	const policyBody = `{"quota_period":"monthly","quota_direction":"rxtx","quota_bytes":4096,"expected_version":0,"reason":"route regression"}`
	assertStatus := func(t *testing.T, w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("want JSON %d: %d %v %s", status, w.Code, w.Header(), w.Body)
		}
	}
	w := f.call("PUT", policyPath, policyBody, uuid.NewString(), f.requester.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", other.String()) })
	assertStatus(t, w, 200)
	var policy useroperations.Policy
	if err := json.Unmarshal(w.Body.Bytes(), &policy); err != nil || policy.Version != 1 || policy.QuotaBytes != 4096 {
		t.Fatalf("policy write: %s %v", w.Body, err)
	}
	var actorID string
	var sessionID uuid.UUID
	if err := f.row(`SELECT actor_id,source_session_id FROM audit_events WHERE node_id=$1 AND action='user.policy.set'`, plan.NodeID).Scan(&actorID, &sessionID); err != nil || actorID != f.requester.principal.IdentityID.String() || sessionID != f.requester.principal.SessionID {
		t.Fatalf("policy audit identity/session: %s %s %v", actorID, sessionID, err)
	}
	// Count successful business intent, not normal rejection audit records.
	counts := func(t *testing.T) [8]int {
		t.Helper()
		var result [8]int
		queries := []string{
			`SELECT count(*) FROM operations WHERE workspace_id=$1`,
			`SELECT count(*) FROM commands WHERE workspace_id=$1`,
			`SELECT count(*) FROM config_plans WHERE workspace_id=$1`,
			`SELECT count(*) FROM config_apply_operations WHERE workspace_id=$1`,
			`SELECT count(*) FROM user_policy_mutations WHERE workspace_id=$1`,
			`SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=$1`,
		}
		for i, query := range queries {
			if err := f.row(query, f.workspace).Scan(&result[i]); err != nil {
				t.Fatal(err)
			}
		}
		result[6], result[7] = int(plans.creates.Load()), int(plans.applies.Load())
		return result
	}
	unchanged := func(t *testing.T, before [8]int) {
		t.Helper()
		if after := counts(t); after != before {
			t.Fatalf("denied request changed business intent: %v -> %v", before, after)
		}
	}
	misleading := func(r *http.Request) {
		r.Header.Set("X-Workspace-ID", other.String())
		r.Header.Set("X-Action", "node.read")
	}

	t.Run("authentication-and-origin", func(t *testing.T) {
		before := counts(t)
		for _, route := range []struct{ method, path string }{
			{"POST", nodePath + "/config-plans"}, {"GET", planPath}, {"POST", planPath + "/apply"},
			{"GET", policyPath}, {"PUT", policyPath},
		} {
			t.Run(route.method+route.path, func(t *testing.T) {
				w := f.call(route.method, route.path, "{", "", nil, nil)
				assertBaselineProblem(t, w, route.path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
				if route.method != "GET" {
					w = f.call(route.method, route.path, "{", "", f.reader.cookie, func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") })
					assertApplyHTTPProblem(t, w, 403, "cross-origin-request")
				}
			})
		}
		unchanged(t, before)
	})
	t.Run("single-admin-and-resource-boundaries", func(t *testing.T) {
		before := counts(t)
		assertStatus(t, f.call("GET", policyPath, "", "", f.requester.cookie, misleading), 200)
		assertStatus(t, f.call("GET", planPath, "", "", f.requester.cookie, misleading), 200)
		// Non-admin user receives 404 on every resource-scoped route so foreign
		// resources never confirm existence.
		for _, route := range []struct{ method, path, body string }{
			{"GET", policyPath, ""}, {"PUT", policyPath, policyBody},
			{"GET", planPath, ""}, {"POST", planPath + "/apply", "{"},
			{"POST", nodePath + "/config-plans", "{"},
		} {
			assertApplyHTTPProblem(t, f.call(route.method, route.path, route.body, uuid.NewString(), f.reader.cookie, nil), 404, "not-found")
		}
		// Admin passes workspace authorization and reaches handler validation.
		for _, route := range []struct{ method, path string }{
			{"POST", nodePath + "/config-plans"}, {"POST", planPath + "/apply"},
		} {
			assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", f.requester.cookie, nil), 400, "idempotency-key-required")
		}
		foreignPath := "/api/v1/nodes/" + foreign.String()
		assertApplyHTTPProblem(t, f.call("GET", foreignPath+"/users/alice/policy", "", "", f.requester.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("PUT", foreignPath+"/users/alice/policy", policyBody, uuid.NewString(), f.requester.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("POST", foreignPath+"/config-plans", "{", uuid.NewString(), f.requester.cookie, nil), 404, "not-found")
		for _, path := range []string{"/api/v1/nodes/invalid/users/alice/policy", "/api/v1/config-plans/" + uuid.Must(uuid.NewV7()).String()} {
			assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 404, "not-found")
		}
		g := f
		g.t, g.workspace = t, other
		foreignPlan := g.plan(false)
		before[6]++
		g.exec(`DELETE FROM workspace_admins WHERE identity_id=$1 AND workspace_id=$2`, f.requester.principal.IdentityID, other)
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/config-plans/"+foreignPlan.ID.String(), "", "", f.requester.cookie, nil), 404, "not-found")
		unchanged(t, before)
	})
	t.Run("secret-resource-boundaries", func(t *testing.T) {
		service := certificates.NewBackend(f.b, f.s.operations, nil, nil, nil, f.signer)
		f.s = f.newServer(Modules{ConfigPlans: plans, UserOperations: userOperations, Certificates: service})
		for _, scope := range []uuid.UUID{f.workspace, other} {
			if scope == other {
				f.bindWorkspaceAdmin(f.requester, other)
			}
			ref, err := service.CreateSecretRef(t.Context(), certificates.SecretRefRequest{WorkspaceID: scope, ActorID: f.requester.principal.IdentityID, SessionID: f.requester.principal.SessionID, Provider: configprofile.Provider, KeyPath: plan.NodeID.String() + "/" + strings.Repeat("52", 32) + "/" + strings.Repeat("53", 32) + "/none", Version: "v1", Reason: "route fixture", RequestID: uuid.NewString()})
			testutil.NoError(t, err)
			if scope == other {
				f.exec(`DELETE FROM workspace_admins WHERE identity_id=$1 AND workspace_id=$2`, f.requester.principal.IdentityID, other)
			}
			body := fmt.Sprintf(`{"template":{"directives":[{"name":"server-key","secret_ref":{"secret_ref_id":%q}}]}}`, ref.ID)
			before := counts(t)
			w := f.call("POST", nodePath+"/config-plans", body, uuid.NewString(), f.requester.cookie, nil)
			if scope == f.workspace {
				assertApplyHTTPProblem(t, w, 400, "config-plan-invalid")
				before[6]++
			} else {
				assertApplyHTTPProblem(t, w, 403, "forbidden")
			}
			unchanged(t, before)
		}
	})
	t.Run("ordinary-methods-and-adjacent-actions", func(t *testing.T) {
		before := counts(t)
		for pattern, allowed := range openAPIExpectedRoutes(t) {
			path := materializeOpenAPIPath(pattern)
			allowHeader := strings.Join(allowed, ", ")
			for _, method := range []string{"GET", "POST", "PUT", "HEAD", "OPTIONS", "BREW"} {
				if slices.Contains(allowed, method) {
					continue
				}
				w := f.call(method, path, "{", uuid.NewString(), f.requester.cookie, func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") })
				assertBaselineProblem(t, w, path, 405, "method-not-allowed", "Method not allowed", "the requested method is not supported")
				if w.Header().Get("Allow") != allowHeader || w.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("%s %s: method headers %v", method, path, w.Header())
				}
			}
		}
		assertStatus(t, f.call("GET", "/livez", "", "", nil, nil), 200)
		assertStatus(t, f.call("GET", nodePath+"/certificates", "", "", f.requester.cookie, nil), 200)
		assertApplyHTTPProblem(t, f.call("POST", nodePath+"/certificates", "{", "", f.requester.cookie, nil), 400, "idempotency-key-required")
		assertApplyHTTPProblem(t, f.call("POST", nodePath+"/certificates", "{", "", f.requester.cookie, func(r *http.Request) { r.Header.Del("Origin") }), 403, "cross-origin-request")
		for _, path := range []string{nodePath + "/users/alice:unknown", nodePath + "/sessions/42:unknown", nodePath + "/ip-bans/192.0.2.9:unknown"} {
			assertApplyHTTPProblem(t, f.call("POST", path, "{", "", f.requester.cookie, nil), 404, "not-found")
		}
		for _, suffix := range []string{"/users/alice:disable", "/sessions/42:terminate", "/ip-bans/192.0.2.9:remove"} {
			assertApplyHTTPProblem(t, f.call("POST", nodePath+suffix, "{", "", f.reader.cookie, nil), 404, "not-found")
		}
		if _, err := f.s.auth.Authenticate(t.Context(), f.requester.cookie); err != nil {
			t.Fatal("method rejection changed the Local session", err)
		}
		unchanged(t, before)
	})
	t.Run("legacy-dynamic-and-sse", func(t *testing.T) {
		before := counts(t)
		for _, suffix := range []string{"/users/alice:disable", "/sessions/42:terminate", "/agent-upgrade"} {
			assertApplyHTTPProblem(t, f.call("POST", nodePath+suffix, "{", "", f.reader.cookie, nil), 404, "not-found")
		}
		f.bindWorkspaceAdmin(f.reader, f.workspace)
		r := baselineRequest("GET", "/api/v1/events/stream", nil)
		r.AddCookie(f.reader.cookie)
		r.Header.Set("X-Workspace-ID", f.workspace.String())
		if !f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("authorized SSE rejected")
		}
		f.exec(`DELETE FROM workspace_admins WHERE workspace_id=$1 AND identity_id=$2`, f.workspace, f.reader.principal.IdentityID)
		if f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("revoked workspace admin retained by SSE")
		}
		f.bindWorkspaceAdmin(f.reader, f.workspace)
		f.exec(`UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, stamp, f.reader.principal.SessionID)
		if f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("revoked session retained by SSE")
		}
		unchanged(t, before)
	})
}
