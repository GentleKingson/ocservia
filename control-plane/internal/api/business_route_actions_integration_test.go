package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

func TestBusinessRouteActionsBackendHTTPIntegration(t *testing.T) {
	// Reuse the restricted-backend, real Local login and HTTP Plan fixture.
	f := newApplyHTTPFixture(t)
	plan := f.plan(false)
	plans := &observedConfigPlans{Plans: f.s.configPlanLookup.(*configplan.Service)}
	f.s.configPlanHTTP.SetPlans(plans)
	userOperations := useroperations.NewBackend(f.b, nil)
	f.s.EnableUserOperations(userOperations)
	f.exec(`DELETE FROM role_bindings WHERE identity_id=$1`, `DELETE FROM role_bindings WHERE identity_id=?`, f.approver.principal.IdentityID)
	f.bind(f.approver, plan.NodeID, "UserManager")
	manager := f.approver
	other, sibling, foreign := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	stamp := value.Timestamp{Valid: true}
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Business routes',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Business routes',?,?,?)`, other, other.String(), stamp, stamp)
	for _, node := range []struct{ id, workspace uuid.UUID }{{sibling, f.workspace}, {foreign, other}} {
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$5)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,?,'active',1,?,?)`, node.id, node.workspace, node.id.String(), stamp, stamp)
	}
	for _, node := range []uuid.UUID{plan.NodeID, sibling, foreign} {
		f.exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES($1,'alice',true,1,1,$2,$3,$4)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES(?,'alice',true,1,1,?,?,?)`, node, make([]byte, 32), stamp, stamp)
	}
	nodePath := "/api/v1/nodes/" + plan.NodeID.String()
	policyPath, planPath := nodePath+"/users/alice/policy", "/api/v1/config-plans/"+plan.ID.String()
	const policyBody = `{"quota_period":"monthly","quota_direction":"rxtx","quota_bytes":4096,"expected_version":0,"reason":"route regression"}`
	batchBody := func(nodes ...uuid.UUID) string {
		var items []useroperations.BatchItemRequest
		for _, node := range nodes {
			items = append(items, useroperations.BatchItemRequest{NodeID: node, Username: "alice", Action: "enable", ExpectedVersion: 1})
		}
		body, err := json.Marshal(map[string]any{"reason": "route regression", "items": items})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	assertStatus := func(t *testing.T, w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("want JSON %d: %d %v %s", status, w.Code, w.Header(), w.Body)
		}
	}
	w := f.call("PUT", policyPath, policyBody, uuid.NewString(), manager.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", other.String()) })
	assertStatus(t, w, 200)
	var policy useroperations.Policy
	if err := json.Unmarshal(w.Body.Bytes(), &policy); err != nil || policy.Version != 1 || policy.QuotaBytes != 4096 {
		t.Fatalf("policy write: %s %v", w.Body, err)
	}
	var actorID string
	var sessionID uuid.UUID
	if err := f.row(`SELECT actor_id,source_session_id FROM audit_events WHERE node_id=$1 AND action='user.policy.set'`, `SELECT actor_id,source_session_id FROM audit_events WHERE node_id=? AND action='user.policy.set'`, plan.NodeID).Scan(&actorID, &sessionID); err != nil || actorID != manager.principal.IdentityID.String() || sessionID != manager.principal.SessionID {
		t.Fatalf("policy audit identity/session: %s %s %v", actorID, sessionID, err)
	}
	w = f.call("POST", "/api/v1/user-batches", batchBody(plan.NodeID), uuid.NewString(), manager.cookie, nil)
	assertStatus(t, w, 202)
	var batch useroperations.Batch
	if err := json.Unmarshal(w.Body.Bytes(), &batch); err != nil || batch.WorkspaceID != f.workspace || len(batch.Items) != 1 || batch.Items[0].State != "queued" {
		t.Fatalf("batch write: %s %v", w.Body, err)
	}
	batchPath := "/api/v1/user-batches/" + batch.ID.String()
	if w.Header().Get("Location") != batchPath {
		t.Fatal("batch Location", w.Header())
	}
	// Count successful business intent, not normal rejection audit records.
	counts := func(t *testing.T) [9]int {
		t.Helper()
		var result [9]int
		queries := [][2]string{
			{`SELECT count(*) FROM operations WHERE workspace_id=$1`, `SELECT count(*) FROM operations WHERE workspace_id=?`},
			{`SELECT count(*) FROM commands WHERE workspace_id=$1`, `SELECT count(*) FROM commands WHERE workspace_id=?`},
			{`SELECT count(*) FROM config_plans WHERE workspace_id=$1`, `SELECT count(*) FROM config_plans WHERE workspace_id=?`},
			{`SELECT count(*) FROM config_apply_operations WHERE workspace_id=$1`, `SELECT count(*) FROM config_apply_operations WHERE workspace_id=?`},
			{`SELECT count(*) FROM user_policy_mutations WHERE workspace_id=$1`, `SELECT count(*) FROM user_policy_mutations WHERE workspace_id=?`},
			{`SELECT count(*) FROM batch_operations WHERE workspace_id=$1`, `SELECT count(*) FROM batch_operations WHERE workspace_id=?`},
			{`SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=$1`, `SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=?`},
		}
		for i, query := range queries {
			if err := f.row(query[0], query[1], f.workspace).Scan(&result[i]); err != nil {
				t.Fatal(err)
			}
		}
		result[7], result[8] = int(plans.creates.Load()), int(plans.applies.Load())
		return result
	}
	unchanged := func(t *testing.T, before [9]int) {
		t.Helper()
		if after := counts(t); after != before {
			t.Fatalf("denied request changed business intent: %v -> %v", before, after)
		}
	}
	bindWorkspace := func(t *testing.T, actor applyHTTPActor, scope uuid.UUID, role string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,$4,'workspace',$5)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,?,'workspace',?)`, id, actor.principal.IdentityID, scope, role, stamp)
		t.Cleanup(func() { f.exec(`DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, id) })
		return id
	}
	misleading := func(r *http.Request) {
		r.Header.Set("X-Workspace-ID", other.String())
		r.Header.Set("X-Action", "node.read")
	}
	noWorkspace := func(r *http.Request) { r.Header.Del("X-Workspace-ID") }

	t.Run("authentication-and-origin", func(t *testing.T) {
		before := counts(t)
		for _, route := range []struct{ method, path string }{
			{"POST", nodePath + "/config-plans"}, {"GET", planPath}, {"POST", planPath + "/apply"},
			{"GET", policyPath}, {"PUT", policyPath}, {"POST", "/api/v1/user-batches"},
			{"GET", batchPath}, {"GET", "/api/v1/user-operations/metrics"},
		} {
			t.Run(route.method+route.path, func(t *testing.T) {
				w := f.call(route.method, route.path, "{", "", nil, nil)
				assertBaselineProblem(t, w, route.path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
				if w.Header().Get("WWW-Authenticate") != "OIDC" {
					t.Fatal("missing authentication challenge")
				}
				if route.method != "GET" {
					// Origin precedes both insufficient permissions and malformed JSON.
					w = f.call(route.method, route.path, "{", "", f.reader.cookie, func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") })
					assertApplyHTTPProblem(t, w, 403, "cross-origin-request")
				}
			})
		}
		unchanged(t, before)
	})
	t.Run("distinct-policy-and-plan-actions", func(t *testing.T) {
		before := counts(t)
		assertStatus(t, f.call("GET", policyPath, "", "", f.reader.cookie, misleading), 200)
		assertStatus(t, f.call("GET", planPath, "", "", f.reader.cookie, misleading), 200)
		for _, route := range []struct{ method, path, body string }{
			{"PUT", policyPath, policyBody}, {"POST", "/api/v1/user-batches", batchBody(plan.NodeID)},
			{"POST", nodePath + "/config-plans", "{"}, {"POST", planPath + "/apply", "{"},
		} {
			w := f.call(route.method, route.path, route.body, uuid.NewString(), f.reader.cookie, nil)
			assertBaselineProblem(t, w, route.path, 403, "forbidden", "Access denied", "the principal is not authorized for this resource and action")
		}
		// node.read alone must not grant config.review.
		assertApplyHTTPProblem(t, f.call("GET", planPath, "", "", manager.cookie, nil), 403, "forbidden")
		for _, route := range []struct {
			method, path string
			actor        applyHTTPActor
		}{
			{"PUT", policyPath, manager}, {"POST", "/api/v1/user-batches", manager},
			{"POST", nodePath + "/config-plans", f.requester}, {"POST", planPath + "/apply", f.requester},
		} {
			// Correct permission reaches the unchanged handler's key-before-body validation.
			assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", route.actor.cookie, nil), 400, "idempotency-key-required")
		}
		unchanged(t, before)
	})
	t.Run("resource-and-workspace-boundaries", func(t *testing.T) {
		before := counts(t)
		for _, node := range []uuid.UUID{sibling, foreign} {
			path := "/api/v1/nodes/" + node.String()
			assertApplyHTTPProblem(t, f.call("GET", path+"/users/alice/policy", "", "", f.reader.cookie, nil), 403, "forbidden")
			assertApplyHTTPProblem(t, f.call("PUT", path+"/users/alice/policy", policyBody, uuid.NewString(), manager.cookie, nil), 403, "forbidden")
			assertApplyHTTPProblem(t, f.call("POST", path+"/config-plans", "{", uuid.NewString(), f.requester.cookie, nil), 403, "forbidden")
		}
		for _, path := range []string{"/api/v1/nodes/invalid/users/alice/policy", "/api/v1/config-plans/" + uuid.Must(uuid.NewV7()).String()} {
			assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 404, "not-found")
		}
		g := f
		g.t, g.workspace = t, other
		foreignPlan := g.plan(false)
		before[7]++ // The foreign-workspace fixture deliberately creates one Plan.
		for _, actor := range []applyHTTPActor{f.reader, manager} {
			f.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2`, `DELETE FROM role_bindings WHERE identity_id=? AND workspace_id=?`, actor.principal.IdentityID, other)
		}
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/config-plans/"+foreignPlan.ID.String(), "", "", f.reader.cookie, nil), 403, "forbidden")
		// A single node-scoped workspace is selectable, but metrics still require
		// workspace-wide operation.read, unlike the batch entry guard.
		assertStatus(t, f.call("GET", batchPath, "", "", manager.cookie, noWorkspace), 200)
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", manager.cookie, nil), 403, "forbidden")
		bindWorkspace(t, manager, other, "UserManager")
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", "{", "", manager.cookie, noWorkspace), 403, "forbidden")
		assertApplyHTTPProblem(t, f.call("GET", batchPath, "", "", manager.cookie, noWorkspace), 403, "forbidden")
		assertBaselineProblem(t, f.call("GET", batchPath, "", "", manager.cookie, misleading), batchPath, 404, "not-found", "Resource not found", "the requested batch does not exist")
		bindWorkspace(t, f.reader, f.workspace, "Viewer")
		assertStatus(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.reader.cookie, noWorkspace), 200)
		bindWorkspace(t, f.reader, other, "Viewer")
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.reader.cookie, noWorkspace), 403, "forbidden")
		assertStatus(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.reader.cookie, nil), 200)
		unchanged(t, before)
	})
	t.Run("batch-per-item-and-reader", func(t *testing.T) {
		before := counts(t)
		w := f.call("POST", "/api/v1/user-batches", batchBody(plan.NodeID, foreign), uuid.NewString(), manager.cookie, nil)
		assertBaselineProblem(t, w, "/api/v1/user-batches", 400, "invalid-request", "Request is invalid", "every batch item must reference an existing user in the selected workspace")
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", strings.ReplaceAll(batchBody(plan.NodeID), `"enable"`, `"disable"`), uuid.NewString(), manager.cookie, nil), 403, "approval-required")
		unchanged(t, before)
		w = f.call("POST", "/api/v1/user-batches", batchBody(plan.NodeID, sibling), uuid.NewString(), manager.cookie, nil)
		assertStatus(t, w, 202)
		var mixed useroperations.Batch
		if err := json.Unmarshal(w.Body.Bytes(), &mixed); err != nil || len(mixed.Items) != 2 || mixed.Items[0].State != "queued" || mixed.Items[1].State != "forbidden" || mixed.Items[1].ErrorType != "forbidden" || mixed.Items[1].ChildOperationID != nil {
			t.Fatalf("per-item authorization: %s %v", w.Body, err)
		}
		path := "/api/v1/user-batches/" + mixed.ID.String()
		assertStatus(t, f.call("GET", path, "", "", manager.cookie, nil), 200)
		stored, err := userOperations.GetBatch(t.Context(), mixed.ID)
		if err != nil || stored.ActorIdentityID == nil || *stored.ActorIdentityID != manager.principal.IdentityID || stored.Items[1].State != "forbidden" {
			t.Fatalf("persisted batch actor/items: %+v %v", stored, err)
		}
		// A node-scoped noncreator passes workspace selection, not the handler's
		// extra workspace-wide read check. A workspace Viewer can read it.
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 403, "forbidden")
		bindWorkspace(t, f.reader, f.workspace, "Viewer")
		assertStatus(t, f.call("GET", path, "", "", f.reader.cookie, nil), 200)
	})
	t.Run("explicit-action-input", func(t *testing.T) {
		bindWorkspace(t, f.reader, f.workspace, "Viewer")
		for _, route := range []struct {
			method, pattern, path, action, denied string
			actor                                 applyHTTPActor
		}{
			{"POST", "/api/v1/nodes/{node_id}/config-plans", nodePath + "/config-plans", "config.plan", "user.manage", f.requester},
			{"GET", "/api/v1/config-plans/{plan_id}", planPath, "config.review", "config.apply", f.reader},
			{"POST", "/api/v1/config-plans/{plan_id}/apply", planPath + "/apply", "config.apply", "user.manage", f.requester},
			{"GET", "/api/v1/nodes/{node_id}/users/{username}/policy", policyPath, "node.read", "config.apply", manager},
			{"PUT", "/api/v1/nodes/{node_id}/users/{username}/policy", policyPath, "user.manage", "config.apply", manager},
			{"POST", "/api/v1/user-batches", "/api/v1/user-batches", "user.manage", "config.apply", manager},
			{"GET", "/api/v1/user-batches/{batch_id}", batchPath, "operation.read", "config.apply", f.reader},
			{"GET", "/api/v1/user-operations/metrics", "/api/v1/user-operations/metrics", "operation.read", "config.apply", f.reader},
		} {
			t.Run(route.method+route.pattern, func(t *testing.T) {
				for _, action := range []string{route.denied, route.action} {
					called := false
					mux := http.NewServeMux()
					mux.HandleFunc(route.method+" "+route.pattern, f.s.requireActionAuth(action, func(w http.ResponseWriter, r *http.Request) {
						called = true
						if workspace(r) != f.workspace || principal(r) != route.actor.principal {
							t.Error("authorized Principal/Workspace context lost")
						}
						w.WriteHeader(204)
					}))
					r := baselineRequest(route.method, route.path+"?action="+route.action, nil)
					r.AddCookie(route.actor.cookie)
					r.Header.Set("Origin", authTestOrigin)
					r.Header.Set("X-Workspace-ID", f.workspace.String())
					r.Header.Set("X-Action", route.action)
					w := httptest.NewRecorder()
					// ServeHTTP performs wildcard matching before the unified guard.
					f.s.requestContext(mux).ServeHTTP(w, r)
					if action == route.denied {
						assertApplyHTTPProblem(t, w, 403, "forbidden")
						if called {
							t.Fatal("denied action entered handler")
						}
					} else if !called || w.Code != 204 {
						t.Fatalf("explicit %s: called=%v %d %s", action, called, w.Code, w.Body)
					}
				}
			})
		}
	})
	t.Run("secret-resource-boundaries", func(t *testing.T) {
		service := certificates.NewBackend(f.b, f.s.operations, nil, nil, nil, f.signer)
		f.s.EnableCertificates(service)
		for _, scope := range []uuid.UUID{f.workspace, other} {
			ref, err := service.CreateSecretRef(t.Context(), certificates.SecretRefRequest{WorkspaceID: scope, ActorID: f.requester.principal.IdentityID, SessionID: f.requester.principal.SessionID, Provider: "fixture", KeyPath: "vpn/key", Version: "v1", Reason: "route fixture", RequestID: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"template":{"directives":[{"name":"key-file","secret_ref":{"secret_ref_id":%q}}]}}`, ref.ID)
			before := counts(t)
			assertApplyHTTPProblem(t, f.call("POST", nodePath+"/config-plans", body, uuid.NewString(), f.requester.cookie, nil), 403, "forbidden")
			// A node ConfigManager has config.plan, not access to arbitrary secrets.
			binding := uuid.Must(uuid.NewV7())
			f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES($1,$2,$3,'ConfigManager','secret_ref',$4,$5)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES(?,?,?,'ConfigManager','secret_ref',?,?)`, binding, f.requester.principal.IdentityID, scope, ref.ID, stamp)
			w := f.call("POST", nodePath+"/config-plans", body, uuid.NewString(), f.requester.cookie, nil)
			if scope == f.workspace {
				assertApplyHTTPProblem(t, w, 400, "config-plan-invalid")
				// This historical assertion intentionally enters domain validation.
				before[7]++
			} else {
				assertApplyHTTPProblem(t, w, 403, "forbidden")
			}
			unchanged(t, before)
		}
	})
	t.Run("legacy-dynamic-and-sse", func(t *testing.T) {
		before := counts(t)
		for _, suffix := range []string{"/users/alice:disable", "/sessions/42:terminate", "/agent-upgrade"} {
			assertApplyHTTPProblem(t, f.call("POST", nodePath+suffix, "{", "", f.reader.cookie, nil), 403, "forbidden")
		}
		// Keep exercising the actual SSE revalidation entry, which intentionally
		// still infers operation.read instead of receiving a registration action.
		binding := bindWorkspace(t, f.reader, f.workspace, "Viewer")
		r := baselineRequest("GET", "/api/v1/events/stream", nil)
		r.AddCookie(f.reader.cookie)
		r.Header.Set("X-Workspace-ID", f.workspace.String())
		if !f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("authorized SSE rejected")
		}
		f.exec(`DELETE FROM role_bindings WHERE id=$1`, `DELETE FROM role_bindings WHERE id=?`, binding)
		if f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("revoked workspace permission retained by SSE")
		}
		bindWorkspace(t, f.reader, f.workspace, "Viewer")
		f.exec(`UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, `UPDATE auth_sessions SET revoked_at=? WHERE id=?`, stamp, f.reader.principal.SessionID)
		if f.s.revalidateEventStream(t.Context(), r, f.reader.principal) {
			t.Fatal("revoked session retained by SSE")
		}
		unchanged(t, before)
	})
}
