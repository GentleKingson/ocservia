package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestHTTPConfigPlanResponseBaselineIntegration(t *testing.T) {
	dsn := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ws, node, secretRef := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, fixture := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'HTTP config baseline',$2,now(),now())`, []any{ws, "http-config-" + ws.String()}},
		{`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'config baseline','active',1,now(),now())`, []any{node, ws}},
		{`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',now())`, []any{node, bytes.Repeat([]byte{42}, 32)}},
		{`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,ocserv,system,path,last_heartbeat_at) VALUES($1,now(),'baseline',$2,'0.7.0','1.2.4','test','{}','{}','{}',now())`, []any{node, uuid.Must(uuid.NewV7())}},
		{`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true),($1,$3,true)`, []any{node, configprofile.PlanCapability, configprofile.ApplyCapability}},
		{`INSERT INTO secret_provider_refs(id,workspace_id,provider,key_path,version,state,created_at,updated_at) VALUES($1,$2,$3,$4,'v1','active',now(),now())`, []any{secretRef, ws, configprofile.Provider, node.String() + "/" + strings.Repeat("42", 32) + "/" + strings.Repeat("43", 32) + "/none"}},
	} {
		if _, err := pool.Exec(ctx, fixture.sql, fixture.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := attestationtest.InstallKey(ctx, pool, node); err != nil {
		t.Fatal(err)
	}
	ops := apiOperationService(pool)
	certs := certificates.NewBackend(postgres.WrapPool(pool), ops, nil, nil, nil, commandauth.NewSignerFromSeed([32]byte{4}))
	s := newTestServer(t, testHTTPConfig(true), postgres.WrapPool(pool), Modules{ConfigPlans: configplan.NewBackend(postgres.WrapPool(pool), ops), Certificates: certs}, Authorization{})
	s.EnableOperations(ops)
	path := "/api/v1/nodes/" + node.String() + "/config-plans"
	body := fmt.Sprintf(`{"expected_revision":0,"template":{"name":"baseline","directives":[`+
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
		`]},"ttl_seconds":900,"reason":"HTTP baseline"}`, secretRef.String(), secretRef.String())
	post := func(key string) *httptest.ResponseRecorder {
		r := baselineRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		return w
	}
	before := time.Now().UTC()
	w := post(" baseline-create ")
	if w.Code != 202 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("create: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var plan configplan.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	created, err := plan.CreatedAt.Time()
	if err != nil {
		t.Fatal(err)
	}
	expires, err := plan.ExpiresAt.Time()
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID.Version() != 7 || plan.ID != plan.OperationID || created.Before(before) || created.After(time.Now().UTC()) || expires.Sub(created) != 15*time.Minute {
		t.Fatalf("generated IDs/times: %+v", plan)
	}
	tlsRef := &agentv1.NodeLocalTlsReference{SecretRefId: secretRef[:], Version: "v1", CertificateSha256: bytes.Repeat([]byte{0x42}, 32), SpkiSha256: bytes.Repeat([]byte{0x43}, 32)}
	candidate := &agentv1.CompleteConfigCandidate{
		NodeId:           node[:],
		ExpectedRevision: 0,
		Directives: []*agentv1.CompleteConfigDirective{
			{Name: "auth", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "plain[passwd=/etc/ocserv/ocpasswd]"}},
			{Name: "cookie-timeout", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "300"}},
			{Name: "device", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "vpns"}},
			{Name: "dns", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "1.1.1.1"}},
			{Name: "ipv4-network", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "10.42.0.0/24"}},
			{Name: "max-clients", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "128"}},
			{Name: "max-same-clients", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "2"}},
			{Name: "server-cert", Value: &agentv1.CompleteConfigDirective_Tls{Tls: tlsRef}},
			{Name: "server-key", Value: &agentv1.CompleteConfigDirective_Tls{Tls: tlsRef}},
			{Name: "socket-file", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "/run/ocserv.socket"}},
			{Name: "tcp-port", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "443"}},
			{Name: "udp-port", Value: &agentv1.CompleteConfigDirective_Literal{Literal: "0"}},
		},
	}
	sum, err := configprofile.Hash(candidate)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", sum)
	// Freeze wire keys independently of Plan's tags, including false values and
	// omitted fields. Only the validated generated IDs and times vary.
	wantMap := map[string]any{
		"id": plan.ID.String(), "operation_id": plan.ID.String(), "workspace_id": ws.String(), "node_id": node.String(),
		"template_name": "baseline", "expected_revision": float64(0), "candidate_hash": wantHash,
		"state": "queued", "validation": "pending", "diff_redacted": "", "warnings": []any{},
		"current_unchanged": false, "staging_cleaned": false,
		"created_at": created.Format(time.RFC3339Nano), "expires_at": expires.Format(time.RFC3339Nano),
	}
	var gotMap map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &gotMap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Fatalf("plan: %s\nwant: %#v", w.Body, wantMap)
	}
	location := "/api/v1/config-plans/" + plan.ID.String()
	if w.Header().Get("Location") != location {
		t.Fatal(w.Header())
	}
	replay := post("baseline-create")
	if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != location || replay.Body.String() != w.Body.String() {
		t.Fatalf("replay: %d %v %s", replay.Code, replay.Header(), replay.Body)
	}
	read := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(read, baselineRequest("GET", location, nil))
	assertBaselineJSON(t, read, w.Body.String())
	operation := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(operation, baselineRequest("GET", "/api/v1/operations/"+plan.ID.String(), nil))
	if operation.Code != 200 || operation.Header().Get("ETag") != `"revision-1"` || operation.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("operation: %d %v %s", operation.Code, operation.Header(), operation.Body)
	}
}
