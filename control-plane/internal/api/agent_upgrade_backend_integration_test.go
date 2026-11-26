package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Run through real HTTP authentication and the restricted runtime on PostgreSQL.
// A single Admin authorizes the trusted target while the intent, command and
// audit stay atomic without an approval record.
func TestAgentUpgradeBackendHTTPIntegration(t *testing.T) {
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	f := newApplyHTTPFixtureWithBackend(t, b, owner)
	node := uuid.MustParse("019fde50-2222-7222-8222-222222222222")
	at := time.Now().UTC()
	f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'upgrade','active',1,$3,$4)`, node, f.workspace, at, at)
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	f.exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, node, []byte(key), at)
	f.exec(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at) VALUES($1,$2,'ed25519',$3,'active',$4,$5,$6)`, node, privdattestation.PublicKeyID(key), []byte(key), at, at, at)
	for _, capability := range []string{"ocserv.agent.upgrade.v2", privdattestation.AttestationCapability} {
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true)`, node, capability)
	}
	manifest := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(manifest, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_format":"deb","package_sha256":"`+strings.Repeat("43", 32)+`"},{"version":"2.0.0","architecture":"arm64","package_format":"deb","package_sha256":"`+strings.Repeat("44", 32)+`"},{"version":"1.0.0","architecture":"amd64","package_format":"deb","package_sha256":"`+strings.Repeat("45", 32)+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	testutil.NoError(t, err)
	f.s = f.newServer(Modules{ReleaseCatalog: catalog})
	f.s.operations.EnableReleaseCatalog(catalog)
	path := "/api/v1/nodes/" + node.String() + "/agent-upgrade"
	body := func(target string) string {
		return fmt.Sprintf(`{"target_version":%q,"reason":"reviewed upgrade","expected_version":1}`, target)
	}
	post := func(target, idempotencyKey string) *httptest.ResponseRecorder {
		return f.call("POST", path, body(target), idempotencyKey, f.requester.cookie, nil)
	}
	problem := func(w *httptest.ResponseRecorder, status int, kind, detail string) {
		t.Helper()
		assertApplyHTTPProblem(t, w, status, kind)
		if detail != "" {
			var p struct {
				Detail string `json:"detail"`
			}
			if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Detail != detail {
				t.Fatalf("problem detail: %s", w.Body)
			}
		}
	}
	setObservation := func(architecture, version string) {
		f.exec(`UPDATE node_observed_snapshots SET architecture=$1,agent_version=$2 WHERE node_id=$3`, architecture, version, node)
	}

	t.Run("input-and-authorization", func(t *testing.T) {
		problem(f.call("POST", path, body("2.0.0"), "key", nil, nil), 401, "unauthenticated", "")
		problem(f.call("POST", path, body("2.0.0"), "key", f.reader.cookie, nil), 404, "not-found", "")
		problem(f.call("POST", path, body("2.0.0"), "", f.requester.cookie, nil), 400, "idempotency-key-required", "")
		problem(f.call("POST", path, `{"target_version":"2.0.0","package_sha256":"untrusted"}`, "key", f.requester.cookie, nil), 400, "invalid-request", "request body is invalid")
		problem(post("invalid", "invalid"), 400, "invalid-request", "target_version and reason are required and ttl_seconds must be between 60 and 3600")
	})
	t.Run("resource-boundaries", func(t *testing.T) {
		otherWorkspace := authSafetyWorkspace(t, owner)
		otherNode := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'other','active',1,$3,$4)`, otherNode, otherWorkspace, at, at)
		problem(f.call("POST", "/api/v1/nodes/"+otherNode.String()+"/agent-upgrade", body("2.0.0"), "foreign", f.requester.cookie, nil), 404, "not-found", "")
	})
	t.Run("target-error-order", func(t *testing.T) {
		problem(post("3.0.0", "unobserved"), 409, "release-not-trusted", "the node has not reported its package architecture yet")
		f.exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,'upgrade',$3,'1.2.0','1.3.0','debian','amd64','{}','{}','{}',$4)`, node, at, uuid.Must(uuid.NewV7()), at)
		// A trusted target reaches the capability fence regardless of the
		// observed version; rejected probes must not create upgrade intents.
		f.exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1`, node)
		for _, tc := range []struct {
			name, arch, observed, target, operationProblem string
		}{
			{"missing-package", "amd64", "1.2.0", "3.0.0", "release-not-trusted"},
			{"unknown-architecture", "riscv64", "1.2.0", "2.0.0", "release-not-trusted"},
			{"unknown-version", "amd64", "unknown", "2.0.0", "capability-unavailable"},
			{"same-version", "amd64", "2.0.0", "2.0.0", "capability-unavailable"},
			{"downgrade", "amd64", "1.2.0", "1.0.0", "capability-unavailable"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setObservation(tc.arch, tc.observed)
				problem(post(tc.target, tc.name), 409, tc.operationProblem, "")
			})
		}
		setObservation("amd64", "1.2.0")
		f.exec(`UPDATE node_capabilities SET approved=true WHERE node_id=$1`, node)
	})
	t.Run("domain-catalog-ownership", func(t *testing.T) {
		for _, tc := range []struct {
			name                                       string
			domainCatalog, httpCatalog, httpOperations bool
		}{
			{name: "missing-http-catalog", domainCatalog: true, httpOperations: true},
			{name: "missing-operations", domainCatalog: true, httpCatalog: true},
			{name: "http-catalog-without-domain-catalog", httpCatalog: true, httpOperations: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				domain := operations.New(b, 50, f.signer)
				if tc.domainCatalog {
					domain.EnableReleaseCatalog(catalog)
				}
				original := f.s
				defer func() { f.s = original }()
				var httpCatalog *releasecatalog.Catalog
				if tc.httpCatalog {
					httpCatalog = catalog
				}
				var httpOperations *operations.Service
				if tc.httpOperations {
					httpOperations = domain
				}
				f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{ReleaseCatalog: httpCatalog, Operations: httpOperations}, Authorization{Authentication: original.auth, Audit: original.audit})
				target, err := domain.PrepareAgentUpgrade(t.Context(), f.workspace, node, "2.0.0")
				if tc.domainCatalog {
					if err != nil || !bytes.Equal(target.PackageSHA256[:], bytes.Repeat([]byte{0x43}, 32)) {
						t.Fatalf("HTTP assembly changed configured domain catalog: %+v %v", target, err)
					}
				} else if !errors.Is(err, operations.ErrUpgradeReleaseNotTrusted) {
					t.Fatalf("HTTP assembly configured domain catalog: %+v %v", target, err)
				}
				if !tc.httpCatalog || !tc.httpOperations {
					problem(post("2.0.0", tc.name), 503, "service-unavailable", "operation service is unavailable")
				} else {
					problem(post("2.0.0", tc.name), 409, "release-not-trusted", "")
				}
			})
		}
	})

	unchanged := func() {
		t.Helper()
		var count int
		if err := f.row(`SELECT count(*) FROM operations WHERE node_id=$1`, node).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected intent: %d %v", count, err)
		}
	}
	t.Run("transaction-rechecks", func(t *testing.T) {
		f.exec(`UPDATE nodes SET version=2 WHERE id=$1`, node)
		problem(post("2.0.0", "stale"), 409, "stale-revision", "")
		f.exec(`UPDATE nodes SET version=1 WHERE id=$1`, node)
		f.exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1`, node)
		problem(post("2.0.0", "missing-capability"), 409, "capability-unavailable", "")
		f.exec(`UPDATE node_capabilities SET approved=true WHERE node_id=$1`, node)
		setObservation("riscv64", "1.2.0")
		problem(post("2.0.0", "changed-architecture"), 409, "release-not-trusted", "")
		setObservation("amd64", "1.2.0")
		unchanged()
	})
	t.Run("atomic-rollback", func(t *testing.T) {
		restore := authSafetyAuditFailure(t, owner, f.workspace, "agent.upgrade")
		defer restore()
		problem(post("2.0.0", "audit-failure"), 503, "database-unavailable", "")
		unchanged()
	})
	t.Run("offline-intent-replay-and-active-conflict", func(t *testing.T) {
		// Single-node operations historically queue for offline nodes. Do not
		// replace this with rollout's stricter online admission policy.
		f.exec(`UPDATE nodes SET status='offline' WHERE id=$1`, node)
		w := post("2.0.0", "accepted")
		var operation operations.Operation
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &operation) != nil || operation.AgentUpgradeState != "queued" || w.Header().Get("Location") != "/api/v1/operations/"+operation.ID || w.Header().Get("ETag") != `"revision-1"` {
			t.Fatalf("accepted: %d %v %s", w.Code, w.Header(), w.Body)
		}
		var payload []byte
		if err := f.row(`SELECT envelope FROM commands WHERE operation_id=$1`, uuid.MustParse(operation.ID)).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		upgrade := envelope.GetAgentUpgrade()
		if envelope.GetRequiredCapability() != "ocserv.agent.upgrade.v2" {
			t.Fatalf("command did not bind the node's actual capability: %q", envelope.GetRequiredCapability())
		}
		if upgrade.GetTargetVersion() != "2.0.0" || upgrade.GetArchitecture() != "amd64" || !bytes.Equal(upgrade.GetPackageSha256(), bytes.Repeat([]byte{0x43}, 32)) {
			t.Fatalf("command release identity: %v", upgrade)
		}
		f.exec(`UPDATE nodes SET version=2 WHERE id=$1`, node)
		replay := post("2.0.0", "accepted")
		if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != w.Header().Get("Location") {
			t.Fatalf("replay: %d %s", replay.Code, replay.Body)
		}
		f.exec(`UPDATE nodes SET version=1 WHERE id=$1`, node)
		problem(post("1.0.0", "accepted"), 409, "idempotency-conflict", "")
		problem(post("2.0.0", "active"), 409, "upgrade-already-active", "")
		var count int
		if err := f.row(`SELECT count(*) FROM agent_upgrade_operations WHERE node_id=$1`, node).Scan(&count); err != nil || count != 1 {
			t.Fatalf("duplicate upgrade intent: %d %v", count, err)
		}
		setObservation("amd64", "2.0.0")
		replay = post("2.0.0", "accepted")
		if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != w.Header().Get("Location") {
			t.Fatalf("replay after version observation changed: %d %s", replay.Code, replay.Body)
		}
	})
}
