package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type applyHTTPActor struct {
	cookie    *http.Cookie
	principal auth.Principal
}

type applyHTTPFixture struct {
	t                           *testing.T
	b, owner                    database.Backend
	s                           *Server
	signer                      *commandauth.Signer
	workspace                   uuid.UUID
	requester, approver, reader applyHTTPActor
}

func newApplyHTTPFixture(t *testing.T) applyHTTPFixture {
	t.Helper()
	b, owner := authenticationBackendFixture(t)
	if owner == nil {
		t.Fatal("owner connection required for isolated Apply fixtures")
	}
	query, _ := authSafetySQL(b, `SELECT current_user`, `SELECT CURRENT_USER()`, nil)
	var user string
	if err := b.QueryRow(t.Context(), query).Scan(&user); err != nil || !strings.HasPrefix(user, "ocservia_app") {
		t.Fatalf("restricted runtime required: %q %v", user, err)
	}
	authn, err := auth.NewBackend(b, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s := NewBackend("127.0.0.1:0", b, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, 15*time.Second, false, "", 36)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.EnableAuthorization(authn, rbac.NewBackend(b), approvals.NewBackend(b), nil)
	s.EnableBrowserOrigin(authTestOrigin)
	signer := commandauth.NewSignerFromSeed([32]byte{4})
	ops := operations.NewBackend(b, 50, signer)
	s.EnableOperations(ops)
	s.EnableConfigPlans(configplan.NewBackend(b, ops))
	f := applyHTTPFixture{t: t, b: b, owner: owner, s: s, signer: signer, workspace: uuid.Must(uuid.NewV7())}
	stamp, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Apply HTTP',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Apply HTTP',?,?,?)`, f.workspace, f.workspace.String(), stamp, stamp)
	login := func() applyHTTPActor {
		name, password := "apply-"+uuid.NewString(), "a unique long Apply HTTP fixture password"
		id, err := authn.CreateLocalCredential(t.Context(), name, password)
		if err != nil {
			t.Fatal(err)
		}
		w := authHTTPRequest(s, "POST", "login", fmt.Sprintf(`{"username":%q,"password":%q}`, name, password), authTestOrigin)
		if w.Code != 204 || len(w.Result().Cookies()) != 1 {
			t.Fatalf("Local login: %d %s", w.Code, w.Body)
		}
		cookie := w.Result().Cookies()[0]
		principal, err := authn.Authenticate(t.Context(), cookie)
		if err != nil || principal.IdentityID != id || principal.SessionID == uuid.Nil || principal.Issuer != auth.LocalIssuer {
			t.Fatalf("real Local principal: %+v %v", principal, err)
		}
		return applyHTTPActor{cookie: cookie, principal: principal}
	}
	f.requester, f.approver, f.reader = login(), login(), login()
	return f
}

func (f applyHTTPFixture) exec(pg, my string, args ...any) {
	f.t.Helper()
	authSafetyExec(f.t, f.owner, pg, my, args...)
}

func (f applyHTTPFixture) row(pg, my string, args ...any) database.Row {
	f.t.Helper()
	query, args := authSafetySQL(f.b, pg, my, args)
	return f.b.QueryRow(f.t.Context(), query, args...)
}

func (f applyHTTPFixture) call(method, path, body, key string, cookie *http.Cookie, mutate func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	r := baselineRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", authTestOrigin)
	r.Header.Set("X-Workspace-ID", f.workspace.String())
	r.Header.Set("Idempotency-Key", key)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	f.s.http.Handler.ServeHTTP(w, r)
	return w
}

func assertApplyHTTPProblem(t *testing.T, w *httptest.ResponseRecorder, status int, kind string) {
	t.Helper()
	var problem struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || w.Code != status || problem.Status != status || problem.Type != "https://ocservia.dev/problems/"+kind || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("X-Request-ID") != "baseline-request" {
		t.Fatalf("want %d %s; got %d %v %s", status, kind, w.Code, w.Header(), w.Body)
	}
	if w.Header().Get("Location") != "" || w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("failure has success headers", w.Header())
	}
}

func (f applyHTTPFixture) bind(actor applyHTTPActor, node uuid.UUID, role string) {
	f.t.Helper()
	f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES($1,$2,$3,$4,'node',$5,$6)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES(?,?,?,?,'node',?,?)`, uuid.Must(uuid.NewV7()), actor.principal.IdentityID, f.workspace, role, node, value.Timestamp{Valid: true})
}

func (f applyHTTPFixture) plan(validated bool) configplan.Plan {
	f.t.Helper()
	node, credential := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	at, err := value.FromTime(now)
	if err != nil {
		f.t.Fatal(err)
	}
	expires, err := value.FromTime(now.Add(time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',3,$4,$5)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,?,'active',3,?,?)`, node, f.workspace, "apply-"+node.String(), at, at)
	f.bind(f.requester, node, "ConfigManager")
	// The requester may review approvals, but independence still forbids self-approval.
	f.bind(f.requester, node, "SecurityAdmin")
	f.bind(f.approver, node, "SecurityAdmin")
	f.bind(f.reader, node, "SecurityAdmin")
	endpoint := sha256.Sum256(node[:])
	private := ed25519.NewKeyFromSeed(endpoint[:])
	public := private.Public().(ed25519.PublicKey)
	f.exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES(?,?,'active',?)`, node, endpoint[:], at)
	f.exec(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, credential, node, endpoint[:], endpoint[:], endpoint[:], expires, at, f.requester.principal.IdentityID, f.requester.principal.SessionID, at)
	f.exec(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id) VALUES($1,$2,'ed25519',$3,'active',$4,$5,$6,$7)`, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id) VALUES(?,?,'ed25519',?,'active',?,?,?,?)`, node, privdattestation.PublicKeyID(public), []byte(public), at, at, at, credential)
	for _, capability := range []string{"ocserv.config.plan", "ocserv.config.apply", "config.network", privdattestation.AttestationCapability} {
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true)`, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,?,true)`, node, capability)
	}
	w := f.call("POST", "/api/v1/nodes/"+node.String()+"/config-plans", `{"expected_revision":0,"template":{"name":"http","directives":[{"name":"tcp-port","value":"${port}"}]},"node_variables":{"port":"443"},"ttl_seconds":900,"reason":"review candidate"}`, uuid.NewString(), f.requester.cookie, nil)
	var plan configplan.Plan
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.NodeID != node || plan.Validation != "pending" || w.Header().Get("Location") != "/api/v1/config-plans/"+plan.ID.String() {
		f.t.Fatalf("create Plan: %d %s", w.Code, w.Body)
	}
	if !validated {
		return plan
	}
	var encoded []byte
	if err := f.row(`SELECT envelope FROM commands WHERE operation_id=$1`, `SELECT envelope FROM commands WHERE operation_id=?`, plan.OperationID).Scan(&encoded); err != nil {
		f.t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE commands SET state='dispatched' WHERE operation_id=$1`, `UPDATE commands SET state='dispatched' WHERE operation_id=?`, plan.OperationID)
	f.exec(`UPDATE operations SET state='dispatched' WHERE id=$1`, `UPDATE operations SET state='dispatched' WHERE id=?`, plan.OperationID)
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: envelope.GetConfigPlan().GetCandidateHash(), CurrentHash: bytes.Repeat([]byte{0x42}, 32), DiffRedacted: "- <current configuration redacted>\n+ tcp-port = 443\n", CurrentUnchanged: true, StagingCleaned: true})
	if err != nil {
		f.t.Fatal(err)
	}
	result := &agentv1.CommandResult{CommandId: envelope.GetCommandId(), IdempotencyKey: envelope.GetIdempotencyKey(), PayloadSha256: envelope.GetSemanticPayloadSha256(), SemanticPayloadHashVersion: envelope.GetSemanticPayloadHashVersion(), State: agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: resultBytes, AcceptedAt: timestamppb.Now(), CompletedAt: timestamppb.Now()}
	if err := attestationtest.AttachProof(&envelope, result, private, 1); err != nil {
		f.t.Fatal(err)
	}
	resultBytes, err = proto.Marshal(result)
	if err != nil {
		f.t.Fatal(err)
	}
	event := uuid.Must(uuid.NewV7())
	// Controlled signed result fixture, not evidence of a real Agent execution.
	if err := localslice.NewBackend(f.b, f.signer).Ingest(f.t.Context(), &transportv1.TransportEvent{EventId: event[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: envelope.GetTraceparent(), Payload: resultBytes}); err != nil {
		f.t.Fatal(err)
	}
	w = f.call("GET", "/api/v1/config-plans/"+plan.ID.String(), "", "", f.requester.cookie, nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Validation != "valid" {
		f.t.Fatalf("validated Plan: %d %s", w.Code, w.Body)
	}
	return plan
}

func (f applyHTTPFixture) approval(plan configplan.Plan, approve bool) approvals.Approval {
	f.t.Helper()
	w := f.call("POST", "/api/v1/approval-requests", fmt.Sprintf(`{"action":"config.apply","resource_type":"config_plan","resource_id":%q,"reason":"review candidate","ttl_seconds":900}`, plan.ID), "", f.requester.cookie, nil)
	var approval approvals.Approval
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &approval) != nil || approval.RequestHash != plan.CandidateHash || approval.ResourceID != plan.ID {
		f.t.Fatalf("request approval: %d %s", w.Code, w.Body)
	}
	if approve {
		w = f.call("POST", "/api/v1/approval-requests/"+approval.ID.String()+":approve", fmt.Sprintf(`{"reason":"independently reviewed","expected_request_hash":%q}`, approval.RequestHash), "", f.approver.cookie, nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &approval) != nil || approval.Status != "approved" || approval.ApproverID == nil || *approval.ApproverID != f.approver.principal.IdentityID {
			f.t.Fatalf("approve: %d %s", w.Code, w.Body)
		}
	}
	return approval
}

func applyHTTPBody(approval uuid.UUID) string {
	return fmt.Sprintf(`{"approval_id":%q,"reason":"apply reviewed configuration"}`, approval)
}

func (f applyHTTPFixture) counts(node uuid.UUID) [5]int {
	f.t.Helper()
	var counts [5]int
	queries := [][2]string{
		{`SELECT count(*) FROM operations WHERE node_id=$1`, `SELECT count(*) FROM operations WHERE node_id=?`},
		{`SELECT count(*) FROM commands WHERE node_id=$1`, `SELECT count(*) FROM commands WHERE node_id=?`},
		{`SELECT count(*) FROM config_apply_operations WHERE node_id=$1`, `SELECT count(*) FROM config_apply_operations WHERE node_id=?`},
		{`SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.node_id=$1`, `SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.node_id=?`},
		{`SELECT count(*) FROM audit_events WHERE node_id=$1 AND action='config.apply' AND result='intent'`, `SELECT count(*) FROM audit_events WHERE node_id=? AND action='config.apply' AND result='intent'`},
	}
	for i, query := range queries {
		if err := f.row(query[0], query[1], node).Scan(&counts[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return counts
}

func (f applyHTTPFixture) unchanged(plan configplan.Plan, before [5]int, approval approvals.Approval) {
	f.t.Helper()
	if got := f.counts(plan.NodeID); got != before {
		f.t.Fatalf("business intent changed: %v -> %v", before, got)
	}
	if approval.ID != uuid.Nil {
		var status string
		if err := f.row(`SELECT status FROM approval_requests WHERE id=$1`, `SELECT status FROM approval_requests WHERE id=?`, approval.ID).Scan(&status); err != nil || status != approval.Status {
			f.t.Fatalf("approval changed: %s -> %s: %v", approval.Status, status, err)
		}
	}
}

func TestConfigPlanApplyBackendHTTPIntegration(t *testing.T) {
	root := newApplyHTTPFixture(t)
	t.Run("noncanonical-paths", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)
		approval := f.approval(plan, true)
		before := f.counts(plan.NodeID)
		server := httptest.NewServer(f.s.http.Handler)
		defer server.Close()
		client := server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		for _, tc := range applyHTTPPathBoundaries(plan.ID.String()) {
			r, err := http.NewRequest("POST", server.URL+tc.path, strings.NewReader(applyHTTPBody(approval.ID)))
			if err != nil {
				t.Fatal(err)
			}
			r.AddCookie(f.requester.cookie)
			r.Header.Set("Origin", authTestOrigin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", uuid.NewString())
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != tc.status || response.Header.Get("Location") != tc.location {
				t.Fatalf("path %s: %d %v %s %v", tc.path, response.StatusCode, response.Header, body, err)
			}
			f.unchanged(plan, before, approval)
		}
		encoded := "/api/v1/config-plans/%30" + plan.ID.String()[1:] + "/apply"
		assertApplyHTTPProblem(t, f.call("POST", encoded, applyHTTPBody(approval.ID), "", f.requester.cookie, nil), 400, "idempotency-key-required")
		f.unchanged(plan, before, approval)
	})
	t.Run("authorization-and-input", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)
		approval := f.approval(plan, true)
		path, body := "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(approval.ID)
		before := f.counts(plan.NodeID)
		for _, tc := range []struct {
			name, body, key, kind string
			status                int
			cookie                *http.Cookie
			mutate                func(*http.Request)
		}{
			{name: "no-session", body: body, key: "denied", kind: "unauthenticated", status: 401},
			{name: "invalid-session", body: body, key: "denied", kind: "unauthenticated", status: 401, cookie: &http.Cookie{Name: auth.SessionCookieName, Value: "invalid"}},
			{name: "missing-origin", body: body, key: "denied", kind: "cross-origin-request", status: 403, cookie: f.requester.cookie, mutate: func(r *http.Request) { r.Header.Del("Origin") }},
			{name: "wrong-origin", body: body, key: "denied", kind: "cross-origin-request", status: 403, cookie: f.requester.cookie, mutate: func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") }},
			{name: "reader-not-applier", body: body, key: "denied", kind: "forbidden", status: 403, cookie: f.reader.cookie},
			{name: "missing-key", body: body, kind: "idempotency-key-required", status: 400, cookie: f.requester.cookie},
			{name: "invalid-json", body: `{`, key: "denied", kind: "invalid-request", status: 400, cookie: f.requester.cookie},
			{name: "unknown-field", body: strings.TrimSuffix(body, "}") + `,"candidate":"replacement"}`, key: "denied", kind: "invalid-request", status: 400, cookie: f.requester.cookie},
			{name: "invalid-approval", body: `{"approval_id":"invalid","reason":"reviewed"}`, key: "denied", kind: "invalid-id", status: 400, cookie: f.requester.cookie},
			{name: "wrong-content-type", body: body, key: "denied", kind: "unsupported-media-type", status: 415, cookie: f.requester.cookie, mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := f
				g.t = t
				w := g.call("POST", path, tc.body, tc.key, tc.cookie, tc.mutate)
				assertApplyHTTPProblem(t, w, tc.status, tc.kind)
				if tc.status == 401 && w.Header().Get("WWW-Authenticate") != "OIDC" {
					t.Fatal("missing challenge")
				}
				g.unchanged(plan, before, approval)
			})
		}
		read := f.call("GET", "/api/v1/config-plans/"+plan.ID.String(), "", "", f.reader.cookie, nil)
		if read.Code != 200 {
			t.Fatalf("reader cannot review: %d %s", read.Code, read.Body)
		}
		t.Run("revoked-session", func(t *testing.T) {
			g := f
			g.t = t
			t.Cleanup(func() {
				g.exec(`UPDATE auth_sessions SET revoked_at=NULL WHERE id=$1`, `UPDATE auth_sessions SET revoked_at=NULL WHERE id=?`, f.requester.principal.SessionID)
			})
			g.exec(`UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, `UPDATE auth_sessions SET revoked_at=? WHERE id=?`, value.Timestamp{Valid: true}, f.requester.principal.SessionID)
			w := g.call("POST", path, body, "revoked-session", f.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, 401, "unauthenticated")
			if w.Header().Get("WWW-Authenticate") != "OIDC" {
				t.Fatal("missing challenge")
			}
			g.unchanged(plan, before, approval)
		})
		for _, id := range []string{"invalid", uuid.NewString(), uuid.Must(uuid.NewV7()).String()} {
			badPath := "/api/v1/config-plans/" + id + "/apply"
			assertApplyHTTPProblem(t, f.call("POST", badPath, body, "denied", nil, nil), 401, "unauthenticated")
			assertApplyHTTPProblem(t, f.call("POST", badPath, body, "denied", f.requester.cookie, nil), 404, "not-found")
		}
		f.unchanged(plan, before, approval)
	})
	t.Run("resource-boundaries", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)
		approval := f.approval(plan, true)
		before := f.counts(plan.NodeID)
		f.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND resource_id=$2`, `DELETE FROM role_bindings WHERE identity_id=? AND resource_id=?`, f.requester.principal.IdentityID, plan.NodeID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(approval.ID), "denied", f.requester.cookie, nil), 403, "forbidden")
		f.unchanged(plan, before, approval)
		f.bind(f.requester, plan.NodeID, "ConfigManager")
		foreign := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'foreign',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'foreign',?,?,?)`, foreign, foreign.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
		g := f
		g.workspace = foreign
		foreignPlan := g.plan(true)
		foreignApproval := g.approval(foreignPlan, true)
		foreignBefore := g.counts(foreignPlan.NodeID)
		g.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2`, `DELETE FROM role_bindings WHERE identity_id=? AND workspace_id=?`, f.requester.principal.IdentityID, foreign)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+foreignPlan.ID.String()+"/apply", applyHTTPBody(foreignApproval.ID), "denied", f.requester.cookie, nil), 403, "forbidden")
		g.unchanged(foreignPlan, foreignBefore, foreignApproval)
		// A misleading header cannot move the authorized Plan to another workspace.
		key := uuid.NewString()
		w := f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(approval.ID), key, f.requester.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", foreign.String()) })
		f.submitted(w, plan, approval, key)
	})
	t.Run("plan-and-approval-rejections", func(t *testing.T) {
		for _, name := range []string{"unvalidated", "expired-plan", "stale-revision", "pending-approval", "expired-approval", "self-approval", "other-plan", "other-content"} {
			t.Run(name, func(t *testing.T) {
				f := root
				f.t = t
				plan := f.plan(name != "unvalidated")
				approval := approvals.Approval{ID: uuid.Must(uuid.NewV7())}
				if name != "unvalidated" {
					approval = f.approval(plan, name != "pending-approval" && name != "self-approval")
				}
				kind := "approval-not-ready"
				switch name {
				case "unvalidated":
					kind = "stale-revision"
				case "expired-plan":
					kind = "stale-revision"
					f.exec(`UPDATE config_plans SET expires_at=$1 WHERE id=$2`, `UPDATE config_plans SET expires_at=? WHERE id=?`, value.Timestamp{Valid: true}, plan.ID)
				case "stale-revision":
					kind = "stale-revision"
					f.exec(`INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,updated_at) VALUES($1,1,1,'',$2)`, `INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,updated_at) VALUES(?,1,1,'',?)`, plan.NodeID, value.Timestamp{Valid: true})
				case "expired-approval":
					f.exec(`UPDATE approval_requests SET created_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE approval_requests SET created_at=?,expires_at=? WHERE id=?`, value.Timestamp{Valid: true}, value.Timestamp{Valid: true, Micros: 1}, approval.ID)
				case "self-approval":
					w := f.call("POST", "/api/v1/approval-requests/"+approval.ID.String()+":approve", fmt.Sprintf(`{"reason":"self","expected_request_hash":%q}`, approval.RequestHash), "", f.requester.cookie, nil)
					assertApplyHTTPProblem(t, w, 403, "self-approval-forbidden")
				case "other-plan":
					approval = f.approval(f.plan(true), true)
				case "other-content":
					f.exec(`UPDATE approval_requests SET request_hash=$1 WHERE id=$2`, `UPDATE approval_requests SET request_hash=? WHERE id=?`, bytes.Repeat([]byte{0x55}, 32), approval.ID)
				}
				before := f.counts(plan.NodeID)
				w := f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(approval.ID), uuid.NewString(), f.requester.cookie, nil)
				assertApplyHTTPProblem(t, w, 409, kind)
				if name == "unvalidated" {
					approval.ID = uuid.Nil
				}
				f.unchanged(plan, before, approval)
			})
		}
	})
	t.Run("rollback-submit-replay", func(t *testing.T) {
		f := root
		f.t = t
		// Keep the audit failure constraint disjoint from earlier successful Applies.
		f.workspace = uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Apply rollback',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Apply rollback',?,?,?)`, f.workspace, f.workspace.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
		plan := f.plan(true)
		approval := f.approval(plan, true)
		path, body, key := "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(approval.ID), uuid.NewString()
		before := f.counts(plan.NodeID)
		t.Run("atomic-rollback", func(t *testing.T) {
			g := f
			g.t = t
			restore := authSafetyAuditFailure(t, g.owner, g.workspace, "config.apply")
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, 503, "config-plan-unavailable")
			g.unchanged(plan, before, approval)
			var desired int64
			if err := g.row(`SELECT COALESCE((SELECT desired_revision FROM node_config_state WHERE node_id=$1),0)`, `SELECT COALESCE((SELECT desired_revision FROM node_config_state WHERE node_id=?),0)`, plan.NodeID).Scan(&desired); err != nil || desired != 0 {
				t.Fatalf("revision rollback: %d %v", desired, err)
			}
			restore()
		})
		w := f.call("POST", path, body, key, f.requester.cookie, nil)
		op := f.submitted(w, plan, approval, key)
		after := f.counts(plan.NodeID)
		for i, delta := range [5]int{1, 1, 1, 1, 1} {
			if after[i] != before[i]+delta {
				t.Fatalf("commit counts: %v -> %v", before, after)
			}
		}
		approval.Status = "consumed"
		t.Run("immediate-replay", func(t *testing.T) {
			g := f
			g.t = t
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			var replay operations.Operation
			if w.Code != 202 || w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("Location") != "/api/v1/operations/"+op.ID || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.ID != op.ID {
				t.Fatalf("immediate replay: %d %v %s", w.Code, w.Header(), w.Body)
			}
			g.unchanged(plan, after, approval)
		})
		t.Run("node-version-drift-replay", func(t *testing.T) {
			g := f
			g.t = t
			var originalVersion, committedVersion int64
			if err := g.row(`SELECT n.version,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=$1`, `SELECT n.version,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=?`, uuid.MustParse(op.ID)).Scan(&originalVersion, &committedVersion); err != nil || originalVersion != committedVersion {
				t.Fatalf("initial node/command versions: %d/%d %v", originalVersion, committedVersion, err)
			}
			event := uuid.Must(uuid.NewV7())
			endpoint := sha256.Sum256(plan.NodeID[:])
			if err := localslice.NewBackend(g.b, g.signer).Ingest(t.Context(), &transportv1.TransportEvent{
				EventId: event[:], NodeId: plan.NodeID[:], EndpointId: endpoint[:],
				Type:       transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED,
				OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte("connection closed"),
			}); err != nil {
				t.Fatal(err)
			}
			var version, expectedVersion int64
			var status string
			if err := g.row(`SELECT n.version,n.status,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=$1`, `SELECT n.version,n.status,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=?`, uuid.MustParse(op.ID)).Scan(&version, &status, &expectedVersion); err != nil || version != originalVersion+1 || status != "offline" || expectedVersion != committedVersion {
				t.Fatalf("transport node/command versions: %d/%s/%d %v", version, status, expectedVersion, err)
			}
			t.Logf("transport changed node version %d -> %d; committed expected_version remains %d", originalVersion, version, expectedVersion)
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			var replay operations.Operation
			if w.Code != 202 || w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("Location") != "/api/v1/operations/"+op.ID || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.ID != op.ID {
				t.Fatalf("node version drift replay: %d %v %s", w.Code, w.Header(), w.Body)
			}
			g.unchanged(plan, after, approval)
		})
		assertApplyHTTPProblem(t, f.call("POST", path, strings.Replace(body, "apply reviewed configuration", "different intent", 1), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		f.bind(f.reader, plan.NodeID, "ConfigManager")
		assertApplyHTTPProblem(t, f.call("POST", path, body, key, f.reader.cookie, nil), 409, "idempotency-conflict")
		freshApproval := f.approval(plan, true)
		assertApplyHTTPProblem(t, f.call("POST", path, applyHTTPBody(freshApproval.ID), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		assertApplyHTTPProblem(t, f.call("POST", path, applyHTTPBody(freshApproval.ID), uuid.NewString(), f.requester.cookie, nil), 409, "config-apply-active")
		f.unchanged(plan, after, freshApproval)
		otherPlan := f.plan(true)
		otherApproval := f.approval(otherPlan, true)
		otherBefore := f.counts(otherPlan.NodeID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+otherPlan.ID.String()+"/apply", applyHTTPBody(otherApproval.ID), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		f.unchanged(otherPlan, otherBefore, otherApproval)
		f.unchanged(plan, after, approval)
		// Isolate approval reuse from the active-Apply guard, without unconsuming it.
		f.exec(`UPDATE config_apply_operations SET state='failed' WHERE operation_id=$1`, `UPDATE config_apply_operations SET state='failed' WHERE operation_id=?`, uuid.MustParse(op.ID))
		assertApplyHTTPProblem(t, f.call("POST", path, body, uuid.NewString(), f.requester.cookie, nil), 409, "approval-not-ready")
		f.unchanged(plan, after, approval)
	})
}

func (f applyHTTPFixture) submitted(w *httptest.ResponseRecorder, plan configplan.Plan, approval approvals.Approval, key string) operations.Operation {
	f.t.Helper()
	var op operations.Operation
	if w.Code != 202 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Idempotency-Replayed") != "" || json.Unmarshal(w.Body.Bytes(), &op) != nil || op.State != "queued" || op.ConfigApplyState != "queued" || op.NodeID == nil || *op.NodeID != plan.NodeID.String() {
		f.t.Fatalf("Apply: %d %v %s", w.Code, w.Header(), w.Body)
	}
	location := "/api/v1/operations/" + op.ID
	if w.Header().Get("Location") != location {
		f.t.Fatal(w.Header())
	}
	read := f.call("GET", location, "", "", f.requester.cookie, nil)
	var got operations.Operation
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &got) != nil || got.ID != op.ID || got.NodeID == nil || *got.NodeID != plan.NodeID.String() {
		f.t.Fatalf("Operation read: %d %s", read.Code, read.Body)
	}
	var planID, approvalID, node, workspace, session, command uuid.UUID
	var expected, desired int64
	var candidateHash, previousHash, envelopeBytes []byte
	var status, actor, reason, request, trace, idempotency string
	pg := `SELECT x.plan_id,x.approval_id,x.node_id,x.workspace_id,x.expected_revision,x.desired_revision,x.candidate_hash,x.previous_hash,c.envelope,a.status,e.actor_id,e.source_session_id,e.command_id,e.reason,e.request_id,e.trace_id,o.idempotency_key FROM config_apply_operations x JOIN operations o ON o.id=x.operation_id JOIN commands c ON c.operation_id=o.id JOIN approval_requests a ON a.id=x.approval_id JOIN audit_events e ON e.resource_id=o.id AND e.action='config.apply' WHERE o.id=$1`
	my := strings.ReplaceAll(pg, "$1", "?")
	if err := f.row(pg, my, uuid.MustParse(op.ID)).Scan(&planID, &approvalID, &node, &workspace, &expected, &desired, &candidateHash, &previousHash, &envelopeBytes, &status, &actor, &session, &command, &reason, &request, &trace, &idempotency); err != nil {
		f.t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
		f.t.Fatal(err)
	}
	claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
	if err != nil {
		f.t.Fatal(err)
	}
	canonical, err := commandauth.CanonicalV1(claims)
	if err != nil || !ed25519.Verify(f.signer.PublicKey(), canonical, envelope.GetAuthorization().GetSignature()) || claims.ApprovalID == nil || *claims.ApprovalID != [16]byte(approval.ID) || claims.ApprovalRequestSHA256 == nil || fmt.Sprintf("%x", *claims.ApprovalRequestSHA256) != plan.CandidateHash {
		f.t.Fatalf("command authorization/approval proof: %+v %v", claims, err)
	}
	apply := envelope.GetConfigApply()
	wantCandidate := []byte("# generated by ocservia config-plan/v1\ntcp-port = 443\n")
	if planID != plan.ID || approvalID != approval.ID || node != plan.NodeID || workspace != plan.WorkspaceID || expected != plan.ExpectedRevision || desired != 1 || fmt.Sprintf("%x", candidateHash) != plan.CandidateHash || fmt.Sprintf("%x", previousHash) != plan.CurrentHash || status != "consumed" || actor != f.requester.principal.IdentityID.String() || session != f.requester.principal.SessionID || reason != "apply reviewed configuration" || request != "baseline-request" || trace != "0123456789abcdef0123456789abcdef" || idempotency != key || apply == nil || !bytes.Equal(apply.GetCandidate(), wantCandidate) || !bytes.Equal(apply.GetCandidateHash(), candidateHash) || !bytes.Equal(apply.GetExpectedCurrentHash(), previousHash) || apply.GetDesiredRevision() != uint64(desired) || !bytes.Equal(envelope.GetNodeId(), node[:]) || !bytes.Equal(envelope.GetCommandId(), command[:]) || envelope.GetActorId() != actor || envelope.GetReason() != reason || !strings.Contains(envelope.GetTraceparent(), trace) {
		f.t.Fatalf("Apply associations or signed command differ: op=%+v envelope=%v actor=%s session=%s", op, &envelope, actor, session)
	}
	return op
}
