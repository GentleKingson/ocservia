package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type applyHTTPActor struct {
	cookie    *http.Cookie
	principal auth.Principal
}

type applyHTTPFixture struct {
	t                 *testing.T
	b, owner          database.Backend
	s                 *Server
	signer            *commandauth.Signer
	workspace         uuid.UUID
	requester, reader applyHTTPActor
}

func newApplyHTTPFixture(t *testing.T) applyHTTPFixture {
	t.Helper()
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	return newApplyHTTPFixtureWithBackend(t, b, owner)
}

func newApplyHTTPFixtureWithBackend(t *testing.T, b, owner database.Backend) applyHTTPFixture {
	t.Helper()
	if owner == nil {
		t.Fatal("owner connection required for isolated Apply fixtures")
	}
	var user string
	if err := b.QueryRow(t.Context(), `SELECT current_user`).Scan(&user); err != nil || !strings.HasPrefix(user, "ocservia_app") {
		t.Fatalf("restricted runtime required: %q %v", user, err)
	}
	authn, err := auth.NewBackend(b, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	signer := commandauth.NewSignerFromSeed([32]byte{4})
	ops := operations.NewBackend(b, 50, signer)
	certs := certificates.NewBackend(b, ops, nil, nil, nil, signer)
	s := newTestServer(t, testHTTPConfig(false), b, Modules{Operations: ops, ConfigPlans: configplan.NewBackend(b, ops), Certificates: certs}, Authorization{Authentication: authn})
	f := applyHTTPFixture{t: t, b: b, owner: owner, s: s, signer: signer, workspace: uuid.Must(uuid.NewV7())}
	stamp, err := value.FromTime(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Apply HTTP',$2,$3,$4)`, f.workspace, f.workspace.String(), stamp, stamp)
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
	f.requester, f.reader = login(), login()
	f.bindWorkspaceAdmin(f.requester, f.workspace)
	return f
}

// Construct another Server over the same persisted fixture and shared services.
// Each scenario owns new modules, admission counters and hubs.
func (f applyHTTPFixture) newServer(modules Modules) *Server {
	modules.Operations = f.s.operations
	s := newTestServer(f.t, testHTTPConfig(false), f.b, modules, Authorization{Authentication: f.s.auth, Audit: f.s.audit})
	return s
}

func (f applyHTTPFixture) exec(query string, args ...any) {
	f.t.Helper()
	authSafetyExec(f.t, f.owner, query, args...)
}

func (f applyHTTPFixture) row(query string, args ...any) database.Row {
	f.t.Helper()
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

func (f applyHTTPFixture) bindWorkspaceAdmin(actor applyHTTPActor, workspaceID uuid.UUID) {
	f.t.Helper()
	f.exec(`INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, workspaceID, actor.principal.IdentityID, value.Timestamp{Valid: true})
}

func (f applyHTTPFixture) plan(validated bool) configplan.Plan {
	f.t.Helper()
	node, secretRef := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := time.Now().UTC().Truncate(time.Microsecond)
	at, err := value.FromTime(now)
	if err != nil {
		f.t.Fatal(err)
	}
	f.bindWorkspaceAdmin(f.requester, f.workspace)
	f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',3,$4,$5)`, node, f.workspace, "apply-"+node.String(), at, at)
	endpoint := sha256.Sum256(node[:])
	private := ed25519.NewKeyFromSeed(endpoint[:])
	public := private.Public().(ed25519.PublicKey)
	f.exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, node, endpoint[:], at)
	f.exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,'apply',$3,'0.7.0','1.2.4','test','{}','{}','{}',$4)`, node, at, uuid.Must(uuid.NewV7()), at)
	f.exec(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at) VALUES($1,$2,'ed25519',$3,'active',$4,$5,$6)`, node, privdattestation.PublicKeyID(public), []byte(public), at, at, at)
	for _, capability := range []string{configprofile.PlanCapability, configprofile.ApplyCapability, "config.network", privdattestation.AttestationCapability} {
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true)`, node, capability)
	}
	f.exec(`INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES($1,$2,$3,$4,'v1','active',$5,$6)`, secretRef, f.workspace, configprofile.Provider, node.String()+"/"+strings.Repeat("42", 32)+"/"+strings.Repeat("43", 32)+"/none", at, at)
	planBody := fmt.Sprintf(`{"expected_revision":0,"template":{"name":"http","directives":[`+
		`{"name":"auth","value":"plain[passwd=/etc/ocserv/ocpasswd]"},`+
		`{"name":"cookie-timeout","value":"300"},`+
		`{"name":"device","value":"vpns"},`+
		`{"name":"dns","value":"1.1.1.1"},`+
		`{"name":"ipv4-network","value":"10.42.0.0/24"},`+
		`{"name":"max-clients","value":"128"},`+
		`{"name":"max-same-clients","value":"2"},`+
		`{"name":"server-cert","secret_ref":{"secret_ref_id":%q}},`+
		`{"name":"server-key","secret_ref":{"secret_ref_id":%q}},`+
		`{"name":"socket-file","value":"/run/ocserv.socket"},`+
		`{"name":"tcp-port","value":"443"},`+
		`{"name":"udp-port","value":"0"}`+
		`]},"ttl_seconds":900,"reason":"review candidate"}`, secretRef.String(), secretRef.String())
	w := f.call("POST", "/api/v1/nodes/"+node.String()+"/config-plans", planBody, uuid.NewString(), f.requester.cookie, nil)
	var plan configplan.Plan
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.NodeID != node || plan.Validation != "pending" || w.Header().Get("Location") != "/api/v1/config-plans/"+plan.ID.String() {
		f.t.Fatalf("create Plan: %d %s", w.Code, w.Body)
	}
	if !validated {
		return plan
	}
	var encoded []byte
	var candidateRedacted string
	if err := f.row(`SELECT c.envelope,p.candidate_redacted FROM commands c JOIN config_plans p ON p.operation_id=c.operation_id WHERE c.operation_id=$1`, plan.OperationID).Scan(&encoded, &candidateRedacted); err != nil {
		f.t.Fatal(err)
	}
	var envelope agentv1.CommandEnvelope
	if err := proto.Unmarshal(encoded, &envelope); err != nil {
		f.t.Fatal(err)
	}
	var diffRedacted strings.Builder
	diffRedacted.WriteString("- <current configuration redacted>\n")
	for _, line := range strings.Split(strings.TrimSuffix(candidateRedacted, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			diffRedacted.WriteString("+ " + line + "\n")
		}
	}
	f.exec(`UPDATE commands SET state='dispatched' WHERE operation_id=$1`, plan.OperationID)
	f.exec(`UPDATE operations SET state='dispatched' WHERE id=$1`, plan.OperationID)
	resultBytes, err := proto.Marshal(&agentv1.ConfigPlanResult{CandidateHash: envelope.GetCompleteConfigPlan().GetCandidateHash(), MaterializedHash: bytes.Repeat([]byte{0x41}, 32), CurrentHash: bytes.Repeat([]byte{0x42}, 32), DiffRedacted: diffRedacted.String(), CurrentUnchanged: true, StagingCleaned: true})
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
	if err := transportevents.NewBackend(f.b, f.signer).Ingest(f.t.Context(), &transportv1.TransportEvent{EventId: event[:], NodeId: node[:], EndpointId: endpoint[:], Type: transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT, OccurredAt: timestamppb.Now(), Traceparent: envelope.GetTraceparent(), Payload: resultBytes}); err != nil {
		f.t.Fatal(err)
	}
	w = f.call("GET", "/api/v1/config-plans/"+plan.ID.String(), "", "", f.requester.cookie, nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Validation != "valid" {
		f.t.Fatalf("validated Plan: %d %s", w.Code, w.Body)
	}
	return plan
}

func applyHTTPBody() string {
	return `{"reason":"apply reviewed configuration"}`
}

func (f applyHTTPFixture) counts(node uuid.UUID) [5]int {
	f.t.Helper()
	var counts [5]int
	queries := []string{
		`SELECT count(*) FROM operations WHERE node_id=$1`,
		`SELECT count(*) FROM commands WHERE node_id=$1`,
		`SELECT count(*) FROM config_apply_operations WHERE node_id=$1`,
		`SELECT count(*) FROM outbox_events b JOIN commands c ON c.id=b.command_id WHERE c.node_id=$1`,
		`SELECT count(*) FROM audit_events WHERE node_id=$1 AND action='config.apply' AND result='intent'`,
	}
	for i, query := range queries {
		if err := f.row(query, node).Scan(&counts[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return counts
}

func (f applyHTTPFixture) unchanged(plan configplan.Plan, before [5]int) {
	f.t.Helper()
	if got := f.counts(plan.NodeID); got != before {
		f.t.Fatalf("business intent changed: %v -> %v", before, got)
	}

}

func TestConfigPlanApplyBackendHTTPIntegration(t *testing.T) {
	root := newApplyHTTPFixture(t)
	t.Run("noncanonical-paths", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)

		before := f.counts(plan.NodeID)
		server := httptest.NewServer(f.s.http.Handler)
		defer server.Close()
		client := server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		for _, tc := range applyHTTPPathBoundaries(plan.ID.String()) {
			r, err := http.NewRequest("POST", server.URL+tc.path, strings.NewReader(applyHTTPBody()))
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
			f.unchanged(plan, before)
		}
		encoded := "/api/v1/config-plans/%30" + plan.ID.String()[1:] + "/apply"
		assertApplyHTTPProblem(t, f.call("POST", encoded, applyHTTPBody(), "", f.requester.cookie, nil), 400, "idempotency-key-required")
		f.unchanged(plan, before)
	})
	t.Run("authorization-and-input", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)

		path, body := "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody()
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
			{name: "unauthorized-user", body: body, key: "denied", kind: "not-found", status: 404, cookie: f.reader.cookie},
			{name: "missing-key", body: body, kind: "idempotency-key-required", status: 400, cookie: f.requester.cookie},
			{name: "invalid-json", body: `{`, key: "denied", kind: "invalid-request", status: 400, cookie: f.requester.cookie},
			{name: "unknown-field", body: strings.TrimSuffix(body, "}") + `,"candidate":"replacement"}`, key: "denied", kind: "invalid-request", status: 400, cookie: f.requester.cookie},
			{name: "wrong-content-type", body: body, key: "denied", kind: "unsupported-media-type", status: 415, cookie: f.requester.cookie, mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := f
				g.t = t
				w := g.call("POST", path, tc.body, tc.key, tc.cookie, tc.mutate)
				assertApplyHTTPProblem(t, w, tc.status, tc.kind)
				g.unchanged(plan, before)
			})
		}
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/config-plans/"+plan.ID.String(), "", "", f.reader.cookie, nil), 404, "not-found")
		t.Run("revoked-session", func(t *testing.T) {
			g := f
			g.t = t
			t.Cleanup(func() {
				g.exec(`UPDATE auth_sessions SET revoked_at=NULL WHERE id=$1`, f.requester.principal.SessionID)
			})
			g.exec(`UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, value.Timestamp{Valid: true}, f.requester.principal.SessionID)
			w := g.call("POST", path, body, "revoked-session", f.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, 401, "unauthenticated")
			g.unchanged(plan, before)
		})
		for _, id := range []string{"invalid", uuid.NewString(), uuid.Must(uuid.NewV7()).String()} {
			badPath := "/api/v1/config-plans/" + id + "/apply"
			assertApplyHTTPProblem(t, f.call("POST", badPath, body, "denied", nil, nil), 401, "unauthenticated")
			assertApplyHTTPProblem(t, f.call("POST", badPath, body, "denied", f.requester.cookie, nil), 404, "not-found")
		}
		f.unchanged(plan, before)
	})
	t.Run("resource-boundaries", func(t *testing.T) {
		f := root
		f.t = t
		plan := f.plan(true)

		before := f.counts(plan.NodeID)
		f.exec(`DELETE FROM workspace_admins WHERE identity_id=$1 AND workspace_id=$2`, f.requester.principal.IdentityID, f.workspace)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(), "denied", f.requester.cookie, nil), 404, "not-found")
		f.unchanged(plan, before)
		f.bindWorkspaceAdmin(f.requester, f.workspace)
		foreign := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'foreign',$2,$3,$4)`, foreign, foreign.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
		g := f
		g.workspace = foreign
		foreignPlan := g.plan(true)

		foreignBefore := g.counts(foreignPlan.NodeID)
		g.exec(`DELETE FROM workspace_admins WHERE identity_id=$1 AND workspace_id=$2`, f.requester.principal.IdentityID, foreign)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+foreignPlan.ID.String()+"/apply", applyHTTPBody(), "denied", f.requester.cookie, nil), 404, "not-found")
		g.unchanged(foreignPlan, foreignBefore)
		// A misleading header cannot move the authorized Plan to another workspace.
		key := uuid.NewString()
		w := f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(), key, f.requester.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", foreign.String()) })
		f.submitted(w, plan, key)
	})
	t.Run("plan-rejections", func(t *testing.T) {
		for _, name := range []string{"unvalidated", "expired-plan", "stale-revision"} {
			t.Run(name, func(t *testing.T) {
				f := root
				f.t = t
				plan := f.plan(name != "unvalidated")
				switch name {
				case "expired-plan":
					f.exec(`UPDATE config_plans SET expires_at=$1 WHERE id=$2`, value.Timestamp{Valid: true}, plan.ID)
				case "stale-revision":
					f.exec(`INSERT INTO node_config_state(node_id,revision,desired_revision,redacted_config,updated_at) VALUES($1,1,1,'',$2)`, plan.NodeID, value.Timestamp{Valid: true})
				}
				before := f.counts(plan.NodeID)
				assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(), uuid.NewString(), f.requester.cookie, nil), 409, "stale-revision")
				f.unchanged(plan, before)
			})
		}
	})
	t.Run("rollback-submit-replay", func(t *testing.T) {
		f := root
		f.t = t
		// Keep the audit failure constraint disjoint from earlier successful Applies.
		f.workspace = uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Apply rollback',$2,$3,$4)`, f.workspace, f.workspace.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
		plan := f.plan(true)

		path, body, key := "/api/v1/config-plans/"+plan.ID.String()+"/apply", applyHTTPBody(), uuid.NewString()
		before := f.counts(plan.NodeID)
		t.Run("atomic-rollback", func(t *testing.T) {
			g := f
			g.t = t
			restore := authSafetyAuditFailure(t, g.owner, g.workspace, "config.apply")
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, 503, "config-plan-unavailable")
			g.unchanged(plan, before)
			var desired int64
			if err := g.row(`SELECT COALESCE((SELECT desired_revision FROM node_config_state WHERE node_id=$1),0)`, plan.NodeID).Scan(&desired); err != nil || desired != 0 {
				t.Fatalf("revision rollback: %d %v", desired, err)
			}
			restore()
		})
		w := f.call("POST", path, body, key, f.requester.cookie, nil)
		op := f.submitted(w, plan, key)
		after := f.counts(plan.NodeID)
		for i, delta := range [5]int{1, 1, 1, 1, 1} {
			if after[i] != before[i]+delta {
				t.Fatalf("commit counts: %v -> %v", before, after)
			}
		}
		t.Run("immediate-replay", func(t *testing.T) {
			g := f
			g.t = t
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			var replay operations.Operation
			if w.Code != 202 || w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("Location") != "/api/v1/operations/"+op.ID || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.ID != op.ID {
				t.Fatalf("immediate replay: %d %v %s", w.Code, w.Header(), w.Body)
			}
			g.unchanged(plan, after)
		})
		t.Run("node-version-drift-replay", func(t *testing.T) {
			g := f
			g.t = t
			var originalVersion, committedVersion int64
			if err := g.row(`SELECT n.version,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=$1`, uuid.MustParse(op.ID)).Scan(&originalVersion, &committedVersion); err != nil || originalVersion != committedVersion {
				t.Fatalf("initial node/command versions: %d/%d %v", originalVersion, committedVersion, err)
			}
			event := uuid.Must(uuid.NewV7())
			endpoint := sha256.Sum256(plan.NodeID[:])
			if err := transportevents.NewBackend(g.b, g.signer).Ingest(t.Context(), &transportv1.TransportEvent{
				EventId: event[:], NodeId: plan.NodeID[:], EndpointId: endpoint[:],
				Type:       transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_DISCONNECTED,
				OccurredAt: timestamppb.Now(), Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", Payload: []byte("connection closed"),
			}); err != nil {
				t.Fatal(err)
			}
			var version, expectedVersion int64
			var status string
			if err := g.row(`SELECT n.version,n.status,c.expected_version FROM commands c JOIN nodes n ON n.id=c.node_id WHERE c.operation_id=$1`, uuid.MustParse(op.ID)).Scan(&version, &status, &expectedVersion); err != nil || version != originalVersion+1 || status != "offline" || expectedVersion != committedVersion {
				t.Fatalf("transport node/command versions: %d/%s/%d %v", version, status, expectedVersion, err)
			}
			t.Logf("transport changed node version %d -> %d; committed expected_version remains %d", originalVersion, version, expectedVersion)
			w := g.call("POST", path, body, key, g.requester.cookie, nil)
			var replay operations.Operation
			if w.Code != 202 || w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("Location") != "/api/v1/operations/"+op.ID || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.ID != op.ID {
				t.Fatalf("node version drift replay: %d %v %s", w.Code, w.Header(), w.Body)
			}
			g.unchanged(plan, after)
		})
		assertApplyHTTPProblem(t, f.call("POST", path, strings.Replace(body, "apply reviewed configuration", "different intent", 1), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		f.bindWorkspaceAdmin(f.reader, f.workspace)
		assertApplyHTTPProblem(t, f.call("POST", path, body, key, f.reader.cookie, nil), 409, "idempotency-conflict")

		assertApplyHTTPProblem(t, f.call("POST", path, applyHTTPBody(), uuid.NewString(), f.requester.cookie, nil), 409, "config-apply-active")
		f.unchanged(plan, after)
		otherPlan := f.plan(true)

		otherBefore := f.counts(otherPlan.NodeID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/config-plans/"+otherPlan.ID.String()+"/apply", applyHTTPBody(), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		f.unchanged(otherPlan, otherBefore)
		f.unchanged(plan, after)

	})
}

func (f applyHTTPFixture) submitted(w *httptest.ResponseRecorder, plan configplan.Plan, key string) operations.Operation {
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
	var planID, node, workspace, session, command uuid.UUID
	var expected, desired int64
	var candidateHash, previousHash, envelopeBytes []byte
	var actor, reason, request, trace, idempotency string
	pg := `SELECT x.plan_id,x.node_id,x.workspace_id,x.expected_revision,x.desired_revision,x.candidate_hash,x.previous_hash,c.envelope,e.actor_id,e.source_session_id,e.command_id,e.reason,e.request_id,e.trace_id,o.idempotency_key FROM config_apply_operations x JOIN operations o ON o.id=x.operation_id JOIN commands c ON c.operation_id=o.id JOIN audit_events e ON e.resource_id=o.id AND e.action='config.apply' WHERE o.id=$1`
	if err := f.row(pg, uuid.MustParse(op.ID)).Scan(&planID, &node, &workspace, &expected, &desired, &candidateHash, &previousHash, &envelopeBytes, &actor, &session, &command, &reason, &request, &trace, &idempotency); err != nil {
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
	if err != nil || !ed25519.Verify(f.signer.PublicKey(), canonical, envelope.GetAuthorization().GetSignature()) || claims.ApprovalID != nil || claims.ApprovalRequestSHA256 != nil {
		f.t.Fatalf("single-admin command authorization: %+v %v", claims, err)
	}
	apply := envelope.GetCompleteConfigApply()
	candidateHashGot, hashErr := configprofile.Hash(apply.GetCandidate())
	if planID != plan.ID || node != plan.NodeID || workspace != plan.WorkspaceID || expected != plan.ExpectedRevision || desired != 1 || fmt.Sprintf("%x", candidateHash) != plan.CandidateHash || fmt.Sprintf("%x", previousHash) != plan.CurrentHash || actor != f.requester.principal.IdentityID.String() || session != f.requester.principal.SessionID || reason != "apply reviewed configuration" || request != "baseline-request" || trace != "0123456789abcdef0123456789abcdef" || idempotency != key || apply == nil || hashErr != nil || !bytes.Equal(candidateHashGot[:], candidateHash) || !bytes.Equal(apply.GetCandidateHash(), candidateHash) || !bytes.Equal(apply.GetExpectedCurrentHash(), previousHash) || apply.GetDesiredRevision() != uint64(desired) || !bytes.Equal(envelope.GetNodeId(), node[:]) || !bytes.Equal(envelope.GetCommandId(), command[:]) || envelope.GetActorId() != actor || envelope.GetReason() != reason || !strings.Contains(envelope.GetTraceparent(), trace) {
		f.t.Fatalf("Apply associations or signed command differ: op=%+v envelope=%v actor=%s session=%s", op, &envelope, actor, session)
	}
	return op
}
