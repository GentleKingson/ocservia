package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestConfigPlanModuleBackendHTTPIntegration(t *testing.T) {
	f := newApplyHTTPFixture(t)
	plan := f.plan(true)
	service := f.s.configPlanLookup.(*configplan.Service)
	handler := f.s.configPlanHTTP
	path := "/api/v1/nodes/" + plan.NodeID.String() + "/config-plans"
	plainBody := `{"expected_revision":0,"template":{"name":"module","directives":[{"name":"tcp-port","value":"443"}]},"ttl_seconds":900,"reason":"module request"}`
	t.Run("late-injection-and-nil", func(t *testing.T) {
		f.s.EnableConfigPlans(nil)
		if f.s.configPlanLookup != nil || f.s.configPlanHTTP != handler {
			t.Fatal("typed nil or replaced Handler")
		}
		for _, route := range []struct {
			method, path string
			status       int
			kind         string
		}{
			{"POST", path, 503, "service-unavailable"},
			{"GET", "/api/v1/config-plans/" + plan.ID.String(), 404, "not-found"},
			{"POST", "/api/v1/config-plans/" + plan.ID.String() + "/apply", 404, "not-found"},
		} {
			assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", nil, nil), 401, "unauthenticated")
			assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", f.requester.cookie, nil), route.status, route.kind)
			if route.method == "POST" {
				assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", f.requester.cookie, func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.example") }), 403, "cross-origin-request")
			}
		}
		f.s.EnableConfigPlans(service)
		if f.s.configPlanLookup != service || f.s.configPlanHTTP != handler {
			t.Fatal("same startup instance not restored")
		}
		w := f.call("GET", "/api/v1/config-plans/"+plan.ID.String(), "", "", f.requester.cookie, nil)
		if w.Code != 200 {
			t.Fatalf("restored read: %d %s", w.Code, w.Body)
		}
		// Certificates is still absent; a request without SecretRef must submit.
		w = f.call("POST", path, plainBody, uuid.NewString(), f.requester.cookie, nil)
		if w.Code != 202 {
			t.Fatalf("restored Create without Secrets: %d %s", w.Code, w.Body)
		}
	})
	plans := &observedConfigPlans{Plans: service}
	f.s.configPlanHTTP.SetPlans(plans)
	secretBody := func(ids ...uuid.UUID) string {
		directives := []configplan.Directive{}
		for i, id := range ids {
			directives = append(directives, configplan.Directive{Name: []string{"server-key", "server-cert"}[i], SecretRef: &configplan.SecretRef{ID: id}})
		}
		body, err := json.Marshal(map[string]any{"expected_revision": 0, "template": configplan.Template{Name: "module", Directives: directives}, "ttl_seconds": 900, "reason": "module secret request"})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	t.Run("secret-service-unconfigured", func(t *testing.T) {
		assertApplyHTTPProblem(t, f.call("POST", path, secretBody(uuid.Must(uuid.NewV7())), uuid.NewString(), f.requester.cookie, nil), 503, "service-unavailable")
		if plans.creates.Load() != 0 {
			t.Fatal("missing Secret service reached Create")
		}
	})
	// Production enables Certificates after ConfigPlans and route registration.
	certs := certificates.NewBackend(f.b, f.s.operations, nil, nil, nil, f.signer)
	f.s.EnableCertificates(certs)
	foreign := uuid.Must(uuid.NewV7())
	stamp := value.Timestamp{Valid: true}
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Secret module',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Secret module',?,?,?)`, foreign, foreign.String(), stamp, stamp)
	f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'config.tls',true)`, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,'config.tls',true)`, plan.NodeID)
	refs := []uuid.UUID{}
	for i, scope := range []uuid.UUID{f.workspace, f.workspace, foreign} {
		keyPath := "vpn/key"
		if i == 1 {
			keyPath = "vpn/denied"
		}
		ref, err := certs.CreateSecretRef(t.Context(), certificates.SecretRefRequest{WorkspaceID: scope, ActorID: f.requester.principal.IdentityID, SessionID: f.requester.principal.SessionID, Provider: "fixture", KeyPath: keyPath, Version: "v1", Reason: "module fixture", RequestID: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref.ID)
	}
	// The first and foreign references have explicit secret.use. The middle one does not.
	for i, scope := range []uuid.UUID{f.workspace, foreign} {
		f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES($1,$2,$3,'ConfigManager','secret_ref',$4,$5)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES(?,?,?,'ConfigManager','secret_ref',?,?)`, uuid.Must(uuid.NewV7()), f.requester.principal.IdentityID, scope, refs[i*2], stamp)
	}
	t.Run("secret-denials-stop-before-create", func(t *testing.T) {
		before := f.counts(plan.NodeID)
		for _, ids := range [][]uuid.UUID{{uuid.Must(uuid.NewV7())}, {refs[1]}, {refs[2]}, {refs[0], refs[1]}, {refs[1], refs[0]}} {
			assertApplyHTTPProblem(t, f.call("POST", path, secretBody(ids...), uuid.NewString(), f.requester.cookie, nil), 403, "forbidden")
		}
		if plans.creates.Load() != 0 || f.counts(plan.NodeID) != before {
			t.Fatal("Secret denial submitted business intent")
		}
	})
	t.Run("valid-secret-submission", func(t *testing.T) {
		key := uuid.NewString()
		w := f.call("POST", path, secretBody(refs[0]), key, f.requester.cookie, func(r *http.Request) {
			r.Header.Set("X-Actor-ID", "forged")
			r.Header.Set("X-Session-ID", uuid.NewString())
			r.Header.Set("X-Workspace-ID", foreign.String())
		})
		var created configplan.Plan
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.WorkspaceID != f.workspace || created.NodeID != plan.NodeID || created.Validation != "pending" || w.Header().Get("Location") != "/api/v1/config-plans/"+created.ID.String() || plans.creates.Load() != 1 {
			t.Fatalf("valid Secret submission: %d %s", w.Code, w.Body)
		}
		var envelopeBytes []byte
		var actor, request, trace, reason, storedKey string
		var session uuid.UUID
		pg := `SELECT c.envelope,e.actor_id,e.source_session_id,e.request_id,e.trace_id,e.reason,o.idempotency_key FROM commands c JOIN operations o ON o.id=c.operation_id JOIN audit_events e ON e.resource_id=o.id AND e.action='config.plan' WHERE o.id=$1`
		if err := f.row(pg, strings.ReplaceAll(pg, "$1", "?"), created.OperationID).Scan(&envelopeBytes, &actor, &session, &request, &trace, &reason, &storedKey); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(envelopeBytes, &envelope); err != nil {
			t.Fatal(err)
		}
		claims, err := commandauth.ClaimsFromEnvelopeV1(&envelope)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := commandauth.CanonicalV1(claims)
		if err != nil || !ed25519.Verify(f.signer.PublicKey(), canonical, envelope.GetAuthorization().GetSignature()) {
			t.Fatal("command signature", err)
		}
		candidate := envelope.GetConfigPlan()
		if candidate == nil || !bytes.Equal(candidate.GetCandidate(), []byte("# generated by ocservia config-plan/v1\nserver-key = ${secret:fixture:vpn/key}\n")) || fmt.Sprintf("%x", candidate.GetCandidateHash()) != created.CandidateHash || actor != f.requester.principal.IdentityID.String() || envelope.GetActorId() != actor || session != f.requester.principal.SessionID || request != "baseline-request" || trace != "0123456789abcdef0123456789abcdef" || !strings.Contains(envelope.GetTraceparent(), trace) || reason != "module secret request" || storedKey != key {
			t.Fatalf("Secret command/audit identity changed: %v %s %s", &envelope, actor, session)
		}
		replay := f.call("POST", path, secretBody(refs[0]), " "+key+" ", f.requester.cookie, nil)
		if replay.Code != 202 || replay.Body.String() != w.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != w.Header().Get("Location") {
			t.Fatalf("Secret replay: %d %s", replay.Code, replay.Body)
		}
	})
	t.Run("development-mode-secret-check", func(t *testing.T) {
		// Adapter-level check only: the real Local principal has no secret.use.
		// The switch, not the Principal issuer, controls this historical exception.
		r := baselineRequest("POST", path, nil)
		ctx := context.WithValue(r.Context(), principalKey{}, f.reader.principal)
		r = r.WithContext(context.WithValue(ctx, workspaceKey{}, f.workspace))
		w := httptest.NewRecorder()
		if f.s.allowConfigPlanSecret(w, r, refs[0]) || w.Code != 403 {
			t.Fatal("Local principal gained secret.use")
		}
		f.s.devAuth = true
		defer func() { f.s.devAuth = false }()
		if !f.s.allowConfigPlanSecret(httptest.NewRecorder(), r, refs[0]) {
			t.Fatal("development mode was replaced by issuer check")
		}
	})
}
