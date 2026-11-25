package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
)

type userOperationsHTTPFixture struct {
	applyHTTPFixture
	service       *useroperations.Service
	node, sibling uuid.UUID
}

func newUserOperationsHTTPFixture(t *testing.T) userOperationsHTTPFixture {
	t.Helper()
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	f := userOperationsHTTPFixture{applyHTTPFixture: newApplyHTTPFixtureWithBackend(t, b, owner)}
	users := userstate.NewWithSignerBackend(b, f.signer)
	f.service = useroperations.NewWithConcurrencyBackend(b, users, 1)
	f.s = f.newServer(Modules{ConfigPlans: f.s.configPlans, UserOperations: f.service, Certificates: f.s.certificates})
	f.node, f.sibling = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, node := range []uuid.UUID{f.node, f.sibling} {
		stamp := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$5)`, node, f.workspace, node.String(), stamp, stamp)
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.users.write',true)`, node)
		if _, err := testfixture.InstallKeyBackend(t.Context(), owner, node); err != nil {
			t.Fatal(err)
		}
		for _, user := range []struct {
			name    string
			version int64
		}{{"alice", 1}, {"bob", 3}} {
			f.exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,created_at,updated_at) VALUES($1,$2,true,$3,1,$4,$5)`, node, user.name, user.version, stamp, stamp)
		}
	}
	return f
}

func assertUserJSON(t *testing.T, w *httptest.ResponseRecorder, status int, target any) {
	t.Helper()
	if w.Code != status || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("want JSON %d: %d %v %s", status, w.Code, w.Header(), w.Body)
	}
	if target != nil {
		if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
			t.Fatal(err)
		}
	}
}

func (f userOperationsHTTPFixture) counts() [5]int {
	f.t.Helper()
	var counts [5]int
	queries := []string{
		`SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='user.policy.set'`,
		`SELECT count(*) FROM user_policy_mutations WHERE workspace_id=$1`,
		`SELECT count(*) FROM operations WHERE workspace_id=$1`,
		`SELECT count(*) FROM commands WHERE workspace_id=$1`,
		`SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=$1`,
	}
	for i, q := range queries {
		if err := f.row(q, f.workspace).Scan(&counts[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return counts
}

func (f userOperationsHTTPFixture) unchanged(before [5]int) {
	f.t.Helper()
	if after := f.counts(); after != before {
		f.t.Fatalf("business intent changed: %v -> %v", before, after)
	}
}

func TestUserOperationsModuleBackendHTTPIntegration(t *testing.T) {
	f := newUserOperationsHTTPFixture(t)
	t.Run("policy-save-read-replay-and-input", func(t *testing.T) {
		f := f
		f.t = t
		path := "/api/v1/nodes/" + f.node.String() + "/users/bob/policy"
		body := `{"quota_period":"monthly","quota_direction":"rxtx","quota_bytes":4096,"expires_at":"2030-01-02T03:04:05Z","expected_version":0,"reason":" policy HTTP "}`
		key := uuid.NewString()
		before := f.counts()
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("PUT", path, body, key, f.reader.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("PUT", path, body, "", f.requester.cookie, nil), 400, "idempotency-key-required")
		f.unchanged(before)
		w := f.call("PUT", path, body, key, f.requester.cookie, nil)
		var policy useroperations.Policy
		assertUserJSON(t, w, 200, &policy)
		if policy.Version != 1 || policy.ExpiresAt == nil || policy.QuotaBytes != 4096 || w.Header().Get("ETag") != `"revision-1"` || w.Header().Get("Idempotency-Replayed") != "" {
			t.Fatal("policy", w.Header(), policy)
		}
		var got useroperations.Policy
		assertUserJSON(t, f.call("GET", path, "", "", f.requester.cookie, nil), 200, &got)
		if !reflect.DeepEqual(policy, got) {
			t.Fatal("policy read", got)
		}
		after := f.counts()
		if after != ([5]int{before[0] + 1, before[1] + 1, before[2], before[3], before[4]}) {
			t.Fatal("policy must not submit commands", before, after)
		}
		w = f.call("PUT", path, body, key, f.requester.cookie, nil)
		assertUserJSON(t, w, 200, &got)
		if w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("ETag") != `"revision-1"` || !reflect.DeepEqual(policy, got) {
			t.Fatal("immediate policy replay", w.Header(), got)
		}
		f.unchanged(after)
		assertApplyHTTPProblem(t, f.call("PUT", path, strings.Replace(body, "4096", "4097", 1), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		assertApplyHTTPProblem(t, f.call("PUT", path, body, uuid.NewString(), f.requester.cookie, nil), 409, "stale-revision")
		f.unchanged(after)
		for _, expiry := range []string{"", "2030-01-02T03:04:05+00:00", "2030-01-02T03:04:05.1Z", "infinity", "12030-01-02T03:04:05Z"} {
			w := f.call("PUT", path, strings.Replace(body, "2030-01-02T03:04:05Z", expiry, 1), uuid.NewString(), f.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, 400, "invalid-request")
			f.unchanged(after)
			if expiry == "2030-01-02T03:04:05.1Z" && !strings.Contains(w.Body.String(), "failed validation") {
				t.Fatal("fraction must reach domain validation", w.Body)
			}
		}
		for _, bad := range []string{strings.Replace(body, "4096", "-1", 1), strings.Replace(body, "4096", "9007199254740992", 1), strings.Replace(body, "4096", "1.5", 1), body + ` {}`, strings.Replace(body, `"quota_period"`, `"unknown"`, 1)} {
			assertApplyHTTPProblem(t, f.call("PUT", path, bad, uuid.NewString(), f.requester.cookie, nil), 400, "invalid-request")
			f.unchanged(after)
		}
		for i, expiry := range []string{`,"expires_at":null`, "", `,"expires_at":"2000-01-01T00:00:00Z"`} {
			body := fmt.Sprintf(`{"quota_period":"none","quota_direction":"rxtx","quota_bytes":0,"expected_version":%d,"reason":"date paths"%s}`, i+1, expiry)
			w := f.call("PUT", path, body, uuid.NewString(), f.requester.cookie, nil)
			got = useroperations.Policy{}
			assertUserJSON(t, w, 200, &got)
			if got.Version != int64(i+2) || (got.ExpiresAt == nil) != (i < 2) || got.Expired != (i == 2) {
				t.Fatal("date semantics", got)
			}
		}
	})
}

func TestUserOperationsHTTPAssemblyBackendIntegration(t *testing.T) {
	f := newUserOperationsHTTPFixture(t)
	path := "/api/v1/nodes/" + f.node.String() + "/users/alice/policy"
	assertUserJSON(t, f.call("PUT", path, `{"quota_period":"none","quota_direction":"rxtx","quota_bytes":0,"expected_version":0,"reason":"assembly policy"}`, uuid.NewString(), f.requester.cookie, nil), 200, nil)
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%v", enabled), func(t *testing.T) {
			f := f
			f.t = t
			before := f.counts()
			authorization := Authorization{Authentication: f.s.auth, Audit: f.s.audit}
			f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{}, Authorization{})
			assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 401, "unauthenticated")
			modules := Modules{}
			if enabled {
				modules.UserOperations = f.service
			}
			f.s = newTestServer(t, testHTTPConfig(false), f.b, modules, authorization)
			if !enabled {
				for _, method := range []string{"GET", "PUT"} {
					assertApplyHTTPProblem(t, f.call(method, path, "{", "", f.requester.cookie, nil), 503, "service-unavailable")
				}
				assertApplyHTTPProblem(t, f.call("PUT", path, "{", "", f.reader.cookie, nil), 404, "not-found")
				assertApplyHTTPProblem(t, f.call("GET", path, "", "", nil, nil), 401, "unauthenticated")
				assertApplyHTTPProblem(t, f.call("GET", "/api/v1/nodes/bad/users/alice/policy", "", "", f.requester.cookie, nil), 404, "not-found")
				f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{UserOperations: f.service}, authorization)
			}
			f.unchanged(before)
			assertUserJSON(t, f.call("GET", path, "", "", f.requester.cookie, nil), 200, nil)
			var nilService *useroperations.Service
			f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{UserOperations: nilService}, authorization)
			assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 503, "service-unavailable")
		})
	}
}
