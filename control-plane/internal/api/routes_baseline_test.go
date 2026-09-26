package api

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const baselineID = "019fc0a4-6d92-765c-a8a1-4af556614cc3"

// Behavior frozen at de26ea4: pattern | handler/wrapper | method rule | permission.
// PR-03 changes only the five nodehttp handler/wrapper expressions.
// F-1 restores the registered ConfigPlan Apply route's POST method rule.
// R2-01 changes eight business wrappers to fixed, explicit actions.
// R2-02 moves only the three ConfigPlan handler/wrapper expressions.
// "self" identifies endpoint validation, not requireOperationAuth. This is a
// test inventory, never an input to production routing or authorization.
const routeBaseline = `GET /livez|s.live|GET|public
GET /readyz|s.ready|GET|public
GET /version|s.version|GET|public
GET /api/v1/livez|s.live|GET|public
GET /api/v1/readyz|s.ready|GET|public
GET /api/v1/version|s.version|GET|public
GET /api/v1/auth/login|s.limitAuthentication(newAuthAdmission(30, 120, 8), s.login)|GET, POST|self
POST /api/v1/auth/login|s.localLogin|GET, POST|self
GET /api/v1/auth/methods|s.authMethods|GET|public
GET /api/v1/auth/callback|s.limitAuthentication(newAuthAdmission(30, 120, 8), s.callback)|GET|self
POST /api/v1/auth/logout|s.requireOperationAuth(s.logout)|POST|session
POST /api/v1/auth/change-password|s.requireOperationAuth(s.changeLocalPassword)|POST|local-session
POST /api/v1/local-users|s.requireOperationAuth(s.createLocalUser)|POST|local_user.manage
POST /api/v1/local-users/{local_user_action}|s.requireOperationAuth(s.localUserAction)|POST|local_user.manage
POST /api/v1/auth/break-glass|s.breakGlass|POST|self
POST /api/v1/development/simulations|s.createSimulation|POST|self
GET /api/v1/development/runtime|s.developmentRuntime|GET|self
GET /api/v1/operations|s.requireOperationAuth(s.listOperations)|GET|operation.read
GET /api/v1/operations/summary|s.requireOperationAuth(s.operationSummary)|GET|operation.read
GET /api/v1/operations/{operation_id}|s.requireOperationAuth(s.getOperation)|GET|operation.read
GET /api/v1/operations/{operation_id}/events|s.requireOperationAuth(s.streamOperationEvents)|GET|operation.read
GET /api/v1/operations/queue-metrics|s.requireOperationAuth(s.queueMetrics)|GET|operation.read
POST /api/v1/nodes/{node_id}/synthetic-commands|s.requireOperationAuth(s.createSyntheticCommand)|POST|operation.create
POST /api/v1/nodes/{node_id}/sessions/{session_action}|s.requireOperationAuth(s.sessionAction)|POST|session.disconnect
POST /api/v1/nodes/{node_id}/ip-bans/{ip_action}|s.requireOperationAuth(s.ipBanAction)|POST|ip_ban.remove
POST /api/v1/nodes/{node_id}/service:reload|s.requireOperationAuth(s.reloadService)|POST|service.reload
POST /api/v1/nodes/{node_id}/agent-upgrade|s.requireOperationAuth(s.upgradeAgent)|POST|agent.upgrade
GET /api/v1/events|s.requireOperationAuth(s.listEvents)|GET|operation.read
GET /api/v1/events/stream|s.requireOperationAuth(s.streamEvents)|GET|operation.read
POST /api/v1/enrollment-tokens|s.requireOperationAuth(s.createEnrollmentToken)|POST|enrollment_token.create
POST /api/v1/node-bootstrap-tokens|s.requireOperationAuth(s.createNodeBootstrapToken)|POST|node_bootstrap_token.create
POST /api/v1/nodes/{node_id}/approval|s.requireOperationAuth(s.approveNode)|POST|node.approve
POST /api/v1/nodes/{node_id}/revocation|s.requireOperationAuth(s.revokeNode)|POST|node.revoke
POST /api/v1/nodes/{node_id}/privd-attestation-credentials|s.requireOperationAuth(s.createPrivdAttestationCredential)|POST|privd.attestation.manage
POST /api/v1/nodes/{node_id}/privd-attestation-keys:register|s.registerPrivdAttestationKey|POST|self
POST /api/v1/nodes/{node_id}/privd-attestation-keys:revoke|s.requireOperationAuth(s.revokePrivdAttestationKey)|POST|privd.attestation.manage
GET /api/v1/nodes|guard("node.read", h.listNodes)|GET|node.read
GET /api/v1/nodes/{node_id}|guard("node.read", h.getNode)|GET|node.read
GET /api/v1/nodes/{node_id}/sessions|guard("node.read", h.listNodeSessions)|GET|node.read
GET /api/v1/nodes/{node_id}/ip-bans|guard("node.read", h.listNodeIPBans)|GET|node.read
GET /api/v1/nodes/{node_id}/telemetry|guard("node.read", h.listNodeTelemetry)|GET|node.read
GET /api/v1/nodes/{node_id}/user-group-state|s.requireOperationAuth(s.listUserGroupState)|GET|node.read
POST /api/v1/nodes/{node_id}/users|s.requireOperationAuth(s.createUser)|POST|user.manage
POST /api/v1/nodes/{node_id}/users/{user_action}|s.requireOperationAuth(s.userAction)|POST|user.manage
PUT /api/v1/nodes/{node_id}/groups/{group_name}|s.requireOperationAuth(s.applyGroup)|PUT|group.manage
GET /api/v1/nodes/{node_id}/users/{username}/policy|guard("node.read", h.getUserPolicy)|GET, PUT|node.read
PUT /api/v1/nodes/{node_id}/users/{username}/policy|guard("user.manage", h.setUserPolicy)|GET, PUT|user.manage
POST /api/v1/user-batches|guard("user.manage", h.createUserBatch)|POST|user.manage
GET /api/v1/user-batches/{batch_id}|guard("operation.read", h.getUserBatch)|GET|operation.read
POST /api/v1/agent-rollouts|s.requireOperationAuth(s.createAgentRollout)|GET, POST|agent.upgrade
GET /api/v1/agent-rollouts|s.requireOperationAuth(s.listAgentRollouts)|GET, POST|operation.read
GET /api/v1/agent-rollouts/{rollout_id}|s.requireOperationAuth(s.getAgentRollout)|GET|operation.read
POST /api/v1/agent-rollouts/{rollout_id}/resume|s.requireOperationAuth(s.resumeAgentRollout)|POST|agent.upgrade
GET /api/v1/user-operations/metrics|guard("operation.read", h.userOperationMetrics)|GET|operation.read
POST /api/v1/nodes/{node_id}/config-plans|guard("config.plan", h.createConfigPlan)|POST|config.plan
GET /api/v1/config-plans/{plan_id}|guard("config.review", h.getConfigPlan)|GET|config.review
POST /api/v1/config-plans/{plan_id}/apply|guard("config.apply", h.applyConfigPlan)|POST|config.apply
POST /api/v1/nodes/{node_id}/certificates|s.requireOperationAuth(s.createCertificate)|GET, POST|certificate.issue
GET /api/v1/nodes/{node_id}/certificates|s.requireOperationAuth(s.listNodeCertificates)|GET, POST|certificate.read
GET /api/v1/certificates/{certificate_id}|s.requireOperationAuth(s.getCertificate)|GET|certificate.read
POST /api/v1/certificates/{certificate_action}|s.requireOperationAuth(s.certificateAction)|POST|certificate.issue
GET /api/v1/artifacts/{artifact_id}|s.requireOperationAuth(s.downloadArtifact)|GET|certificate.private_key.export
POST /api/v1/secret-provider-refs|s.requireOperationAuth(s.createSecretRef)|POST|secret.manage
GET /api/v1/secret-provider-refs/{secret_ref_id}|s.requireOperationAuth(s.getSecretRef)|GET|secret.read
POST /api/v1/secret-provider-refs/{secret_ref_action}|s.requireOperationAuth(s.rotateSecretRef)|POST|secret.manage
POST /api/v1/approval-requests|s.requireOperationAuth(s.createApproval)|POST|approval.request
GET /api/v1/approval-requests/{approval_id}|s.requireOperationAuth(s.getApproval)|GET|approval.approve
POST /api/v1/approval-requests/{approval_id}|s.requireOperationAuth(s.approveRequest)|POST|approval.approve
GET /api/v1/audit/events|s.requireOperationAuth(s.listAuditEvents)|GET|audit.read
POST /api/v1/audit:verify|s.requireOperationAuth(s.verifyAudit)|POST|audit.verify
GET /api/v1/workspaces|s.requireOperationAuth(s.listWorkspaces)|GET|session
POST /api/v1/role-bindings|s.requireOperationAuth(s.createRoleBinding)|POST|role_binding.manage`

const compatibilityBaseline = `POST /api/v1/local-users/{local_user_action}
GET /api/v1/operations/{operation_id}
POST /api/v1/nodes/{node_id}/sessions/{session_action}
POST /api/v1/nodes/{node_id}/ip-bans/{ip_action}
POST /api/v1/nodes/{node_id}/users/{user_action}
GET /api/v1/certificates/{certificate_id}
POST /api/v1/certificates/{certificate_action}
GET /api/v1/secret-provider-refs/{secret_ref_id}
POST /api/v1/secret-provider-refs/{secret_ref_action}
GET /api/v1/approval-requests/{approval_id}
POST /api/v1/approval-requests/{approval_id}`

func baselineServer(t *testing.T, dev bool) *Server {
	t.Helper()
	s := NewBackend("127.0.0.1:0", nil, BuildInfo{Version: "baseline", Commit: "fixture", Role: "api"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, dev, "")
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func baselineRequest(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("X-Request-ID", "baseline-request")
	// A fixed span makes Problem bodies and log correlation deterministic without
	// removing trace IDs or other fields from comparisons.
	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	return r.WithContext(trace.ContextWithSpanContext(r.Context(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})))
}

func baselineRoutePath(pattern, handler string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.NewReplacer("{local_user_action}", baselineID+":disable", "{session_action}", "42:disconnect", "{ip_action}", "192.0.2.9:remove", "{user_action}", "alice:disable", "{certificate_action}", baselineID+":issue", "{secret_ref_action}", baselineID+":rotate", "{group_name}", "operators", "{username}", "alice").Replace(path)
	for strings.Contains(path, "{") {
		start, end := strings.Index(path, "{"), strings.Index(path, "}")
		path = path[:start] + baselineID + path[end+1:]
	}
	if handler == "s.requireOperationAuth(s.approveRequest)" {
		path += ":approve"
	}
	return method, path
}

func assertBaselineProblem(t *testing.T, w *httptest.ResponseRecorder, path string, status int, kind, title, detail string) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("X-Request-ID") != "baseline-request" {
		t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "https://ocservia.dev/problems/" + kind, "title": title, "detail": detail, "status": float64(status), "instance": path, "trace_id": "0123456789abcdef0123456789abcdef"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Problem = %#v, want %#v", got, want)
	}
}

// Recognize only the single, direct, unchanged forwarding call in the exact
// construction registrar method. Other nonliteral route declarations fail.
func isMethodForwarder(fset *token.FileSet, name string, file *ast.File, call *ast.CallExpr) bool {
	if name != "routing.go" {
		return false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "HandleFunc" || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
			continue
		}
		field := fn.Recv.List[0]
		if len(field.Names) != 1 || field.Names[0].Name != "r" {
			continue
		}
		var receiver, signature, expression bytes.Buffer
		if format.Node(&receiver, fset, field.Type) != nil || format.Node(&signature, fset, fn.Type) != nil || format.Node(&expression, fset, call) != nil {
			return false
		}
		if receiver.String() != "*methodRegistrar" || signature.String() != "func(pattern string, handler func(http.ResponseWriter, *http.Request))" || expression.String() != "r.mux.HandleFunc(pattern, handler)" {
			continue
		}
		for _, stmt := range fn.Body.List {
			if expr, ok := stmt.(*ast.ExprStmt); ok && expr.X == call {
				return true
			}
		}
	}
	return false
}

func TestHTTPRouteInventory(t *testing.T) {
	// Check only literal HandleFunc registrations, including wrapper arguments.
	// No production metadata or second runtime permission map is introduced.
	registered := map[string]string{}
	explicitActions := map[string]string{}
	compatibility := map[string]bool{}
	for _, pattern := range strings.Split(compatibilityBaseline, "\n") {
		compatibility[pattern] = true
	}
	compatibilityRegistrations := 0
	forwarders := 0
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"nodehttp", "configplanhttp", "useroperationshttp"} {
		moduleFiles, err := filepath.Glob(module + "/*.go")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, moduleFiles...)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				if isMethodForwarder(fset, name, file, call) {
					forwarders++
					return true
				}
				t.Fatal("route pattern is no longer literal")
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			var handler bytes.Buffer
			if err := format.Node(&handler, fset, call.Args[1]); err != nil {
				t.Fatal(err)
			}
			if _, exists := registered[pattern]; exists {
				t.Fatalf("duplicate %s", pattern)
			}
			registered[pattern] = handler.String()
			if receiver, ok := sel.X.(*ast.Ident); ok && receiver.Name == "compat" {
				if !compatibility[pattern] {
					t.Fatalf("unexpected compatibility registration: %s", pattern)
				}
				compatibilityRegistrations++
			}
			guard, wrapped := call.Args[1].(*ast.CallExpr)
			var wrapper bytes.Buffer
			if wrapped {
				if err := format.Node(&wrapper, fset, guard.Fun); err != nil {
					t.Fatal(err)
				}
			}
			if filepath.Dir(name) != "." && wrapper.String() != "guard" {
				t.Fatal("module route must declare its guard action")
			}
			if wrapper.String() == "s.requireActionAuth" || wrapper.String() == "guard" {
				if len(guard.Args) != 2 {
					t.Fatal("explicit authorization requires an action and handler")
				}
				action, ok := guard.Args[0].(*ast.BasicLit)
				if !ok || action.Kind != token.STRING {
					t.Fatal("permission is no longer a fixed string")
				}
				explicitActions[pattern], err = strconv.Unquote(action.Value)
				if err != nil || strings.TrimSpace(explicitActions[pattern]) == "" {
					t.Fatal("explicit permission must be nonempty", err)
				}
				if _, ok := guard.Args[1].(*ast.SelectorExpr); !ok {
					t.Fatal("explicit authorization must wrap the original handler")
				}
			}
			return true
		})
	}
	if len(registered) != 72 || len(explicitActions) != 13 {
		t.Fatalf("registrations/actions = %d/%d, want 72/13", len(registered), len(explicitActions))
	}
	if forwarders != 1 {
		t.Fatalf("registration forwarders = %d, want exactly one", forwarders)
	}
	s := baselineServer(t, false)
	derived := 0
	for _, rule := range s.registeredMethods {
		derived += len(rule.methods)
	}
	if derived != 61 || len(s.registeredMethods) != 57 || compatibilityRegistrations != 11 {
		t.Fatalf("derived registrations/shapes/compatibility = %d/%d/%d, want 61/57/11", derived, len(s.registeredMethods), compatibilityRegistrations)
	}
	for _, line := range strings.Split(routeBaseline, "\n") {
		fields := strings.Split(line, "|")
		pattern, handler, allow, permission := fields[0], fields[1], fields[2], fields[3]
		t.Run(pattern, func(t *testing.T) {
			if registered[pattern] != handler {
				t.Fatalf("registration = %q, want %q", registered[pattern], handler)
			}
			delete(registered, pattern)
			method, path := baselineRoutePath(pattern, handler)
			r := baselineRequest(method, path, nil)
			if rule, ok := s.routeMethods(path); !ok || rule.allow() != allow {
				t.Fatalf("registered route is unreachable: %s (%q)", pattern, rule)
			}
			if compatibility[pattern] {
				if _, ok := registeredRouteMethods(s.registeredMethods, path); ok {
					t.Fatalf("compatibility route incorrectly derived: %s", pattern)
				}
				if rule, ok := compatibilityRouteMethod(path); !ok || rule != allow {
					t.Fatalf("compatibility route lost: %s (%q)", pattern, rule)
				}
			} else {
				if rule, ok := registeredRouteMethods(s.registeredMethods, path); !ok || rule.allow() != allow {
					t.Fatalf("ordinary route not derived: %s (%q)", pattern, rule)
				}
				// The detail rule can interpret these two static names as IDs. All
				// other ordinary paths (including the original 13) must be absent.
				if rule, ok := compatibilityRouteMethod(path); ok && path != "/api/v1/operations/summary" && path != "/api/v1/operations/queue-metrics" {
					t.Fatalf("ordinary route still duplicated in compatibility rules: %s (%q)", pattern, rule)
				}
			}
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			if strings.HasPrefix(handler, "s.requireOperationAuth(") || strings.HasPrefix(handler, "s.requireActionAuth(") || strings.HasPrefix(handler, "guard(") {
				assertBaselineProblem(t, w, path, 401, "unauthenticated", "Authentication required", "operation state requires an authenticated principal")
				if w.Header().Get("WWW-Authenticate") != "OIDC" {
					t.Fatal("missing challenge")
				}
				if action, explicit := explicitActions[pattern]; explicit {
					if action != permission {
						t.Fatalf("explicit action = %q, want %q", action, permission)
					}
				} else if permission != "session" && permission != "local-session" && routeAction(r) != permission {
					t.Fatalf("action = %q, want %q", routeAction(r), permission)
				}
			} else {
				switch handler {
				case "s.live":
					if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"status\":\"ok\"}\n" {
						t.Fatalf("live: %d %s", w.Code, w.Body)
					}
				case "s.version":
					if w.Code != 200 || w.Body.String() != "{\"version\":\"baseline\",\"commit\":\"fixture\",\"role\":\"api\"}\n" {
						t.Fatalf("version: %d %s", w.Code, w.Body)
					}
				case "s.authMethods":
					if w.Code != 200 || w.Body.String() != "{\"local\":false,\"oidc\":false}\n" || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("methods: %d %s", w.Code, w.Body)
					}
				case "s.ready":
					assertBaselineProblem(t, w, path, 503, "database-unavailable", "Service is not ready", "database dependency is unavailable")
				default:
					detail := "the requested resource does not exist"
					switch handler {
					case "s.localLogin":
						detail = "Local authentication is not configured"
					case "s.breakGlass":
						detail = "break-glass is not configured"
					case "s.limitAuthentication(newAuthAdmission(30, 120, 8), s.login)", "s.limitAuthentication(newAuthAdmission(30, 120, 8), s.callback)":
						detail = "OIDC authentication is not configured"
					case "s.registerPrivdAttestationKey":
						detail = "privd attestation provisioning is not enabled"
					}
					assertBaselineProblem(t, w, path, 404, "not-found", "Resource not found", detail)
				}
			}
			for _, wrong := range []string{"HEAD", "OPTIONS", "BREW"} {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, baselineRequest(wrong, path, nil))
				assertBaselineProblem(t, w, path, 405, "method-not-allowed", "Method not allowed", "the requested method is not supported")
				if w.Header().Get("Allow") != allow || w.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("method headers: %v", w.Header())
				}
			}
		})
	}
	if len(registered) != 0 {
		t.Fatalf("unrecorded routes: %v", registered)
	}
}
