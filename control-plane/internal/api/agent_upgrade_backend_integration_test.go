package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Run through real HTTP authentication and the restricted runtime on every
// backend. Preparation is not execution authorization: the trusted target
// still requires independent approval, while the intent stays atomic.
func TestAgentUpgradeBackendHTTPIntegration(t *testing.T) {
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	f := newApplyHTTPFixtureWithBackend(t, b, owner)
	node := uuid.MustParse("019fde50-2222-7222-8222-222222222222")
	at, _ := value.FromTime(time.Now().UTC())
	expires, _ := value.FromTime(time.Now().UTC().Add(time.Hour))
	f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'upgrade','active',1,$3,$4)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,'upgrade','active',1,?,?)`, node, f.workspace, at, at)
	f.bind(f.requester, node, "Operator")
	f.bind(f.approver, node, "SecurityAdmin")
	f.bind(f.reader, node, "Viewer")
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	credential := uuid.Must(uuid.NewV7())
	f.exec(`INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES($1,$2,'active',$3)`, `INSERT INTO node_endpoint_keys(node_id,endpoint_id,state,bound_at) VALUES(?,?,'active',?)`, node, []byte(key), at)
	f.exec(`INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, `INSERT INTO privd_attestation_enrollment_credentials(id,node_id,secret_sha256,controller_nonce,credential_context_sha256,expires_at,consumed_at,created_by_identity_id,created_by_session_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, credential, node, []byte(key), []byte(key), []byte(key), expires, at, f.requester.principal.IdentityID, f.requester.principal.SessionID, at)
	f.exec(`INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id) VALUES($1,$2,'ed25519',$3,'active',$4,$5,$6,$7)`, `INSERT INTO node_privd_attestation_keys(node_id,key_id,algorithm,public_key,state,created_at,approved_at,activated_at,registration_credential_id) VALUES(?,?,'ed25519',?,'active',?,?,?,?)`, node, privdattestation.PublicKeyID(key), []byte(key), at, at, at, credential)
	for _, capability := range []string{"ocserv.agent.upgrade.v1", privdattestation.AttestationCapability} {
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,$2,true)`, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,?,true)`, node, capability)
	}
	manifest := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(manifest, []byte(`{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"`+strings.Repeat("43", 32)+`"},{"version":"2.0.0","architecture":"arm64","package_sha256":"`+strings.Repeat("44", 32)+`"},{"version":"1.0.0","architecture":"amd64","package_sha256":"`+strings.Repeat("45", 32)+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.s.EnableReleaseCatalog(catalog)
	path := "/api/v1/nodes/" + node.String() + "/agent-upgrade"
	body := func(target string, approval uuid.UUID) string {
		return fmt.Sprintf(`{"target_version":%q,"approval_id":%q,"reason":"reviewed upgrade","expected_version":1}`, target, approval)
	}
	requestApproval := func(target string) *httptest.ResponseRecorder {
		return f.call("POST", "/api/v1/approval-requests", fmt.Sprintf(`{"action":"agent.upgrade","resource_type":"node","resource_id":%q,"reason":"review release","ttl_seconds":900,"agent_upgrade":{"target_version":%q}}`, node, target), "", f.requester.cookie, nil)
	}
	post := func(target string, approval uuid.UUID, idempotencyKey string) *httptest.ResponseRecorder {
		return f.call("POST", path, body(target, approval), idempotencyKey, f.requester.cookie, nil)
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
	approve := func(target string) approvals.Approval {
		t.Helper()
		w := requestApproval(target)
		var approval approvals.Approval
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &approval) != nil {
			t.Fatalf("request approval: %d %s", w.Code, w.Body)
		}
		w = f.call("POST", "/api/v1/approval-requests/"+approval.ID.String()+":approve", fmt.Sprintf(`{"reason":"independent review","expected_request_hash":%q}`, approval.RequestHash), "", f.approver.cookie, nil)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &approval) != nil || approval.Status != "approved" {
			t.Fatalf("approve: %d %s", w.Code, w.Body)
		}
		return approval
	}
	setObservation := func(architecture, version string) {
		f.exec(`UPDATE node_observed_snapshots SET architecture=$1,agent_version=$2 WHERE node_id=$3`, `UPDATE node_observed_snapshots SET architecture=?,agent_version=? WHERE node_id=?`, architecture, version, node)
	}

	t.Run("input-and-authorization", func(t *testing.T) {
		problem(f.call("POST", path, body("2.0.0", uuid.Nil), "key", nil, nil), 401, "unauthenticated", "")
		problem(f.call("POST", path, body("2.0.0", uuid.Nil), "key", f.reader.cookie, nil), 403, "forbidden", "")
		problem(f.call("POST", path, body("2.0.0", uuid.Nil), "", f.requester.cookie, nil), 400, "idempotency-key-required", "")
		problem(f.call("POST", path, `{"target_version":"2.0.0","package_sha256":"untrusted"}`, "key", f.requester.cookie, nil), 400, "invalid-request", "request body is invalid")
		problem(post("invalid", uuid.Must(uuid.NewV7()), "invalid"), 400, "invalid-request", "target_version, approval_id, and reason are required and ttl_seconds must be between 60 and 3600")
		problem(requestApproval("invalid"), 400, "invalid-request", "agent upgrade approval target version is invalid")
	})
	t.Run("resource-boundaries", func(t *testing.T) {
		otherWorkspace := authSafetyWorkspace(t, owner)
		otherNode := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'other','active',1,$3,$4)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,'other','active',1,?,?)`, otherNode, otherWorkspace, at, at)
		problem(f.call("POST", "/api/v1/nodes/"+otherNode.String()+"/agent-upgrade", body("2.0.0", uuid.Must(uuid.NewV7())), "foreign", f.requester.cookie, nil), 403, "forbidden", "")
		problem(f.call("POST", "/api/v1/approval-requests", fmt.Sprintf(`{"action":"agent.upgrade","resource_type":"node","resource_id":%q,"reason":"review release","ttl_seconds":900,"agent_upgrade":{"target_version":"2.0.0"}}`, otherNode), "", f.requester.cookie, nil), 409, "node-not-ready", "agent upgrade approval requires an observed node in the selected workspace")
	})
	t.Run("target-error-order", func(t *testing.T) {
		problem(post("3.0.0", uuid.Must(uuid.NewV7()), "unobserved"), 409, "release-not-trusted", "the node has not reported its package architecture yet")
		problem(requestApproval("3.0.0"), 409, "node-not-ready", "the node has not reported its package architecture yet")
		f.exec(`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at) VALUES($1,$2,'upgrade',$3,'1.2.0','1.3.0','debian','amd64','{}','{}','{}',$4)`, "INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,`system`,path,last_heartbeat_at) VALUES(?,?,'upgrade',?,'1.2.0','1.3.0','debian','amd64','{}','{}','{}',?)", node, at, uuid.Must(uuid.NewV7()), at)
		for _, tc := range []struct {
			name, arch, observed, target, operationProblem string
			approvalStatus                                 int
		}{
			{"missing-package", "amd64", "1.2.0", "3.0.0", "release-not-trusted", 409},
			{"unknown-architecture", "riscv64", "1.2.0", "2.0.0", "release-not-trusted", 409},
			{"unknown-version", "amd64", "unknown", "2.0.0", "approval-required", 201},
			{"same-version", "amd64", "2.0.0", "2.0.0", "approval-required", 201},
			{"downgrade", "amd64", "1.2.0", "1.0.0", "approval-required", 201},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setObservation(tc.arch, tc.observed)
				problem(post(tc.target, uuid.Must(uuid.NewV7()), tc.name), 409, tc.operationProblem, "")
				w := requestApproval(tc.target)
				if tc.approvalStatus == 409 {
					problem(w, 409, "release-not-trusted", "")
				} else if w.Code != 201 {
					t.Fatalf("approval eligibility changed: %d %s", w.Code, w.Body)
				}
			})
		}
		setObservation("amd64", "1.2.0")
	})

	approval := approve(" 2.0.0 ")
	t.Run("approval-binding-golden", func(t *testing.T) {
		summary := `{"action":"agent.upgrade","architecture":"amd64","node_id":"019fde50-2222-7222-8222-222222222222","package_sha256":"4343434343434343434343434343434343434343434343434343434343434343","target_version":"2.0.0"}`
		hash := sha256.Sum256(append([]byte("ocservia/approval-request/agent-upgrade/v1\x00"), summary...))
		var decoded map[string]any
		if err := json.Unmarshal(approval.RequestSummary, &decoded); err != nil {
			t.Fatal(err)
		}
		canonical, _ := json.Marshal(decoded)
		if approval.RequestHash != hex.EncodeToString(hash[:]) || string(canonical) != summary {
			t.Fatalf("binding changed: %s %s", approval.RequestHash, canonical)
		}
	})
	unchanged := func() {
		t.Helper()
		var count int
		if err := f.row(`SELECT count(*) FROM operations WHERE node_id=$1`, `SELECT count(*) FROM operations WHERE node_id=?`, node).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected intent: %d %v", count, err)
		}
		var status string
		if err := f.row(`SELECT status FROM approval_requests WHERE id=$1`, `SELECT status FROM approval_requests WHERE id=?`, approval.ID).Scan(&status); err != nil || status != "approved" {
			t.Fatalf("approval consumed on rejection: %s %v", status, err)
		}
	}
	t.Run("transaction-rechecks", func(t *testing.T) {
		f.exec(`UPDATE nodes SET version=2 WHERE id=$1`, `UPDATE nodes SET version=2 WHERE id=?`, node)
		problem(post("2.0.0", approval.ID, "stale"), 409, "stale-revision", "")
		f.exec(`UPDATE nodes SET version=1 WHERE id=$1`, `UPDATE nodes SET version=1 WHERE id=?`, node)
		f.exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1`, `UPDATE node_capabilities SET approved=false WHERE node_id=?`, node)
		problem(post("2.0.0", approval.ID, "missing-capability"), 409, "capability-unavailable", "")
		f.exec(`UPDATE node_capabilities SET approved=true WHERE node_id=$1`, `UPDATE node_capabilities SET approved=true WHERE node_id=?`, node)
		setObservation("arm64", "1.2.0")
		problem(post("2.0.0", approval.ID, "changed-architecture"), 409, "approval-required", "")
		setObservation("amd64", "1.2.0")
		unchanged()
	})
	t.Run("atomic-rollback", func(t *testing.T) {
		restore := authSafetyAuditFailure(t, owner, f.workspace, "agent.upgrade")
		defer restore()
		problem(post("2.0.0", approval.ID, "audit-failure"), 503, "database-unavailable", "")
		unchanged()
	})
	t.Run("offline-intent-replay-and-active-conflict", func(t *testing.T) {
		// Single-node operations historically queue for offline nodes. Do not
		// replace this with rollout's stricter online admission policy.
		f.exec(`UPDATE nodes SET status='offline' WHERE id=$1`, `UPDATE nodes SET status='offline' WHERE id=?`, node)
		w := post("2.0.0", approval.ID, "accepted")
		var operation operations.Operation
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &operation) != nil || operation.AgentUpgradeState != "queued" || w.Header().Get("Location") != "/api/v1/operations/"+operation.ID || w.Header().Get("ETag") != `"revision-1"` {
			t.Fatalf("accepted: %d %v %s", w.Code, w.Header(), w.Body)
		}
		var payload []byte
		if err := f.row(`SELECT envelope FROM commands WHERE operation_id=$1`, `SELECT envelope FROM commands WHERE operation_id=?`, uuid.MustParse(operation.ID)).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		upgrade := envelope.GetAgentUpgrade()
		if envelope.GetRequiredCapability() != "ocserv.agent.upgrade.v1" {
			t.Fatalf("command did not bind the node's actual capability: %q", envelope.GetRequiredCapability())
		}
		if upgrade.GetTargetVersion() != "2.0.0" || upgrade.GetArchitecture() != "amd64" || !bytes.Equal(upgrade.GetPackageSha256(), bytes.Repeat([]byte{0x43}, 32)) {
			t.Fatalf("command release identity: %v", upgrade)
		}
		f.exec(`UPDATE nodes SET version=2 WHERE id=$1`, `UPDATE nodes SET version=2 WHERE id=?`, node)
		replay := post("2.0.0", approval.ID, "accepted")
		if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != w.Header().Get("Location") {
			t.Fatalf("replay: %d %s", replay.Code, replay.Body)
		}
		f.exec(`UPDATE nodes SET version=1 WHERE id=$1`, `UPDATE nodes SET version=1 WHERE id=?`, node)
		other := approve("2.0.0")
		problem(post("2.0.0", other.ID, "accepted"), 409, "idempotency-conflict", "")
		problem(post("2.0.0", other.ID, "active"), 409, "upgrade-already-active", "")
		var status string
		if err := f.row(`SELECT status FROM approval_requests WHERE id=$1`, `SELECT status FROM approval_requests WHERE id=?`, other.ID).Scan(&status); err != nil || status != "approved" {
			t.Fatalf("active conflict consumed approval: %s %v", status, err)
		}
		var count int
		if err := f.row(`SELECT count(*) FROM agent_upgrade_operations WHERE node_id=$1`, `SELECT count(*) FROM agent_upgrade_operations WHERE node_id=?`, node).Scan(&count); err != nil || count != 1 {
			t.Fatalf("duplicate upgrade intent: %d %v", count, err)
		}
		setObservation("amd64", "2.0.0")
		replay = post("2.0.0", approval.ID, "accepted")
		if replay.Code != 202 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Header().Get("Location") != w.Header().Get("Location") {
			t.Fatalf("replay after version observation changed: %d %s", replay.Code, replay.Body)
		}
	})
}
