package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/coordination"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
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
	f.s = f.newServer(Modules{ConfigPlans: f.s.configPlanLookup.(ConfigPlans), UserOperations: f.service})
	f.node, f.sibling = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, node := range []uuid.UUID{f.node, f.sibling} {
		stamp := value.Timestamp{Valid: true}
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,$4,$5)`, `INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES(?,?,?,'active',1,?,?)`, node, f.workspace, node.String(), stamp, stamp)
		f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.users.write',true)`, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,'ocserv.users.write',true)`, node)
		for _, user := range []struct {
			name    string
			version int64
		}{{"alice", 1}, {"bob", 3}} {
			f.exec(`INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES($1,$2,true,$3,1,$4,$5,$6)`, `INSERT INTO desired_users(node_id,username,enabled,version,revision,fingerprint,created_at,updated_at) VALUES(?,?,true,?,1,?,?,?)`, node, user.name, user.version, make([]byte, 32), stamp, stamp)
		}
		f.bind(f.approver, node, "SecurityAdmin")
	}
	f.bind(f.requester, f.node, "UserManager")
	f.bind(f.reader, f.node, "Viewer")
	return f
}

func userBatchBody(t *testing.T, items []useroperations.BatchItemRequest) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"reason": " batch HTTP ", "items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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

func (f userOperationsHTTPFixture) counts() [7]int {
	f.t.Helper()
	var counts [7]int
	queries := [][2]string{
		{`SELECT count(*) FROM batch_operations WHERE workspace_id=$1`, `SELECT count(*) FROM batch_operations WHERE workspace_id=?`},
		{`SELECT count(*) FROM batch_operation_items i JOIN batch_operations b ON b.id=i.batch_id WHERE b.workspace_id=$1`, `SELECT count(*) FROM batch_operation_items i JOIN batch_operations b ON b.id=i.batch_id WHERE b.workspace_id=?`},
		{`SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action IN ('user.batch.create','user.policy.set')`, `SELECT count(*) FROM audit_events WHERE workspace_id=? AND action IN ('user.batch.create','user.policy.set')`},
		{`SELECT count(*) FROM user_policy_mutations WHERE workspace_id=$1`, `SELECT count(*) FROM user_policy_mutations WHERE workspace_id=?`},
		{`SELECT count(*) FROM operations WHERE workspace_id=$1`, `SELECT count(*) FROM operations WHERE workspace_id=?`},
		{`SELECT count(*) FROM commands WHERE workspace_id=$1`, `SELECT count(*) FROM commands WHERE workspace_id=?`},
		{`SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=$1`, `SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.workspace_id=?`},
	}
	for i, q := range queries {
		if err := f.row(q[0], q[1], f.workspace).Scan(&counts[i]); err != nil {
			f.t.Fatal(err)
		}
	}
	return counts
}

func (f userOperationsHTTPFixture) unchanged(before [7]int) {
	f.t.Helper()
	if after := f.counts(); after != before {
		f.t.Fatalf("business intent changed: %v -> %v", before, after)
	}
}

func (f userOperationsHTTPFixture) approvalState(id uuid.UUID, want string) {
	f.t.Helper()
	var status string
	if err := f.row(`SELECT status FROM approval_requests WHERE id=$1`, `SELECT status FROM approval_requests WHERE id=?`, id).Scan(&status); err != nil || status != want {
		f.t.Fatalf("approval status %s want %s: %v", status, want, err)
	}
}

func (f userOperationsHTTPFixture) approval(items []useroperations.BatchItemRequest, approve bool) approvals.Approval {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{"action": "user.batch.disable", "resource_type": "batch_operation", "batch_items": items, "reason": "independent batch review", "ttl_seconds": 900})
	if err != nil {
		f.t.Fatal(err)
	}
	w := f.call("POST", "/api/v1/approval-requests", string(body), "", f.requester.cookie, nil)
	var approval approvals.Approval
	assertUserJSON(f.t, w, 201, &approval)
	hash := useroperations.BatchRequestHash(items)
	if approval.ResourceID.Version() != 7 || approval.RequestHash != hex.EncodeToString(hash[:]) || approval.RequesterID != f.requester.principal.IdentityID || approval.Status != "pending" {
		f.t.Fatalf("approval binding: %+v", approval)
	}
	if approve {
		decision := fmt.Sprintf(`{"reason":"independent review","expected_request_hash":%q}`, approval.RequestHash)
		w = f.call("POST", "/api/v1/approval-requests/"+approval.ID.String()+":approve", decision, "", f.approver.cookie, nil)
		assertUserJSON(f.t, w, 200, &approval)
		if approval.Status != "approved" || approval.ApproverID == nil || *approval.ApproverID != f.approver.principal.IdentityID || *approval.ApproverID == approval.RequesterID {
			f.t.Fatal("approval independence", approval)
		}
	}
	return approval
}

func approvalHeader(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Approval-ID", id) }
}

func TestUserOperationsModuleBackendHTTPIntegration(t *testing.T) {
	// Use the shared Local/Cookie/Origin fixture with restricted runtime. Only
	// fixture setup and scoped fault injection use owner; no Agent is involved.
	f := newUserOperationsHTTPFixture(t)
	t.Run("approval-rollback-submit-replay-scheduler", func(t *testing.T) {
		f := f
		f.t = t
		items := []useroperations.BatchItemRequest{{NodeID: f.node, Username: "alice", Action: "disable", ExpectedVersion: 1}, {NodeID: f.node, Username: "bob", Action: "enable", ExpectedVersion: 3}}
		body, key := userBatchBody(t, items), uuid.NewString()
		before := f.counts()
		for _, header := range []string{"", "bad", uuid.NewString(), uuid.Must(uuid.NewV7()).String()} {
			assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(header)), 403, "approval-required")
			f.unchanged(before)
		}
		pending := f.approval(items, false)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(pending.ID.String())), 403, "approval-required")
		f.approvalState(pending.ID, "pending")
		f.unchanged(before)
		expired := f.approval(items, true)
		// Expiry is a fixture clock condition, never a direct approval shortcut.
		f.exec(`UPDATE approval_requests SET created_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE approval_requests SET created_at=?,expires_at=? WHERE id=?`, value.Timestamp{Valid: true}, value.Timestamp{Valid: true, Micros: 1}, expired.ID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(expired.ID.String())), 403, "approval-required")
		f.approvalState(expired.ID, "approved")
		f.unchanged(before)
		persistedMismatch := f.approval(items, true)
		f.exec(`UPDATE approval_batch_items SET expected_version=expected_version+1 WHERE approval_id=$1 AND item_index=0`, `UPDATE approval_batch_items SET expected_version=expected_version+1 WHERE approval_id=? AND item_index=0`, persistedMismatch.ID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(persistedMismatch.ID.String())), 403, "approval-required")
		f.approvalState(persistedMismatch.ID, "approved")
		f.unchanged(before)
		approval := f.approval(items, true)
		for _, change := range []func([]useroperations.BatchItemRequest){
			func(v []useroperations.BatchItemRequest) { v[0], v[1] = v[1], v[0] },
			func(v []useroperations.BatchItemRequest) { v[0].ExpectedVersion++ },
			func(v []useroperations.BatchItemRequest) { v[1].Username = "missing" },
		} {
			altered := append([]useroperations.BatchItemRequest(nil), items...)
			change(altered)
			assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", userBatchBody(t, altered), key, f.requester.cookie, approvalHeader(approval.ID.String())), 403, "approval-required")
			f.approvalState(approval.ID, "approved")
			f.unchanged(before)
		}
		t.Run("atomic-rollback", func(t *testing.T) {
			f := f
			f.t = t
			restore := authSafetyAuditFailure(t, f.owner, f.workspace, "user.batch.create")
			assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(approval.ID.String())), 503, "database-unavailable")
			f.unchanged(before)
			f.approvalState(approval.ID, "approved")
			restore()
		})
		w := f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(" "+approval.ID.String()+" "))
		var batch useroperations.Batch
		assertUserJSON(t, w, 202, &batch)
		if batch.ID != approval.ResourceID || w.Header().Get("Location") != "/api/v1/user-batches/"+batch.ID.String() || w.Header().Get("Idempotency-Replayed") != "" || len(batch.Items) != len(items) {
			t.Fatal("approved batch identity", w.Header(), batch)
		}
		for i, item := range batch.Items {
			if item.Index != i || item.NodeID != items[i].NodeID || item.Username != items[i].Username || item.Action != items[i].Action || item.ExpectedVersion != items[i].ExpectedVersion || item.State != "queued" || item.ChildOperationID != nil {
				t.Fatal("batch intent/order", batch.Items)
			}
		}
		f.approvalState(approval.ID, "consumed")
		after := f.counts()
		if want := [7]int{before[0] + 1, before[1] + 2, before[2] + 1, before[3], before[4], before[5], before[6]}; after != want {
			t.Fatalf("HTTP must only save batch intent: %v want %v", after, want)
		}
		var actor, reason, requestID, traceparent, storedKey string
		var identity, session, approvalID uuid.UUID
		if err := f.row(`SELECT actor_id,actor_identity_id,actor_session_id,approval_id,reason,request_id,traceparent,idempotency_key FROM batch_operations WHERE id=$1`, `SELECT actor_id,actor_identity_id,actor_session_id,approval_id,reason,request_id,traceparent,idempotency_key FROM batch_operations WHERE id=?`, batch.ID).Scan(&actor, &identity, &session, &approvalID, &reason, &requestID, &traceparent, &storedKey); err != nil || identity != f.requester.principal.IdentityID || actor != identity.String() || session != f.requester.principal.SessionID || approvalID != approval.ID || reason != "batch HTTP" || requestID != "baseline-request" || storedKey != key || !strings.HasPrefix(traceparent, "00-0123456789abcdef0123456789abcdef-") {
			t.Fatalf("batch provenance: %s %s %s %s %v", actor, session, requestID, traceparent, err)
		}
		var auditActor, auditRequest string
		var auditSession, auditApproval uuid.UUID
		if err := f.row(`SELECT actor_id,source_session_id,approval_id,request_id FROM audit_events WHERE resource_id=$1 AND action='user.batch.create'`, `SELECT actor_id,source_session_id,approval_id,request_id FROM audit_events WHERE resource_id=? AND action='user.batch.create'`, batch.ID).Scan(&auditActor, &auditSession, &auditApproval, &auditRequest); err != nil || auditActor != actor || auditSession != session || auditApproval != approvalID || auditRequest != requestID {
			t.Fatal("batch audit provenance", err)
		}
		t.Run("immediate-replay", func(t *testing.T) {
			f := f
			f.t = t
			w := f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, approvalHeader(approval.ID.String()))
			var replay useroperations.Batch
			assertUserJSON(t, w, 202, &replay)
			if !reflect.DeepEqual(replay, batch) || w.Header().Get("Idempotency-Replayed") != "true" || w.Header().Get("Location") != "/api/v1/user-batches/"+batch.ID.String() {
				t.Fatal("batch replay", w.Header(), replay)
			}
			f.unchanged(after)
			f.approvalState(approval.ID, "consumed")
		})
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", body, uuid.NewString(), f.requester.cookie, approvalHeader(approval.ID.String())), 403, "approval-required")
		f.unchanged(after)
		// Enter the original Service scheduling path separately from HTTP. A
		// real lease/fence and real userstate service create the signed child.
		leaderID, err := coordination.NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		leader, err := coordination.AcquireBackend(t.Context(), f.b, leaderID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ctx := coordination.WithFence(t.Context(), leader)
		if err := f.service.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		stored, err := f.service.GetBatch(t.Context(), batch.ID)
		if err != nil || stored.Items[0].ChildOperationID == nil || stored.Items[1].ChildOperationID != nil {
			t.Fatalf("scheduler association: %+v %v", stored, err)
		}
		child := *stored.Items[0].ChildOperationID
		var command uuid.UUID
		var childRequest, childKey, commandKey, commandTrace, action string
		var encoded []byte
		if err := f.row(`SELECT o.request_id,o.idempotency_key,c.id,c.idempotency_key,c.traceparent,c.payload_type,c.envelope FROM operations o JOIN commands c ON c.operation_id=o.id WHERE o.id=$1`, `SELECT o.request_id,o.idempotency_key,c.id,c.idempotency_key,c.traceparent,c.payload_type,c.envelope FROM operations o JOIN commands c ON c.operation_id=o.id WHERE o.id=?`, child).Scan(&childRequest, &childKey, &command, &commandKey, &commandTrace, &action, &encoded); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte("batch\x00" + batch.ID.String() + "\x000"))
		if childRequest != requestID+":0" || childKey != "i14-"+hex.EncodeToString(digest[:]) || commandKey != childKey || commandTrace != traceparent || action != "user_disable" {
			t.Fatal("child linkage", childRequest, childKey, commandKey, commandTrace, action)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil || envelope.GetUserDisable().GetUsername() != "alice" || envelope.GetTraceparent() != traceparent || envelope.GetActorId() != actor || len(envelope.GetAuthorization().GetSignature()) == 0 || !reflect.DeepEqual(envelope.GetCommandId(), command[:]) {
			t.Fatal("signed command", err, &envelope)
		}
		var outbox, audits int
		if err := f.row(`SELECT count(*) FROM outbox_events WHERE command_id=$1`, `SELECT count(*) FROM outbox_events WHERE command_id=?`, command).Scan(&outbox); err != nil || outbox != 1 {
			t.Fatal("outbox", outbox, err)
		}
		if err := f.row(`SELECT count(*) FROM audit_events WHERE resource_id=$1 AND command_id=$2 AND actor_id=$3 AND source_session_id=$4 AND request_id=$5 AND action='user.disable'`, `SELECT count(*) FROM audit_events WHERE resource_id=? AND command_id=? AND actor_id=? AND source_session_id=? AND request_id=? AND action='user.disable'`, child, command, actor, session, childRequest).Scan(&audits); err != nil || audits != 1 {
			t.Fatal("child audit", audits, err)
		}
		if err := f.service.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		// Queued commands are not the scheduler's active execution budget;
		// the next tick may submit the second item, but must retain the first.
		stored, err = f.service.GetBatch(t.Context(), batch.ID)
		if err != nil || stored.Items[0].ChildOperationID == nil || *stored.Items[0].ChildOperationID != child {
			t.Fatal("scheduler replaced existing child", stored, err)
		}
		var sameKey int
		if err := f.row(`SELECT count(*) FROM commands WHERE workspace_id=$1 AND idempotency_key=$2`, `SELECT count(*) FROM commands WHERE workspace_id=? AND idempotency_key=?`, f.workspace, childKey).Scan(&sameKey); err != nil || sameKey != 1 {
			t.Fatal("scheduler duplicated first child", sameKey, err)
		}
	})
	t.Run("policy-save-read-replay-and-input", func(t *testing.T) {
		f := f
		f.t = t
		path := "/api/v1/nodes/" + f.node.String() + "/users/bob/policy"
		body := `{"quota_period":"monthly","quota_direction":"rxtx","quota_bytes":4096,"expires_at":"2030-01-02T03:04:05Z","expected_version":0,"reason":" policy HTTP "}`
		key := uuid.NewString()
		before := f.counts()
		w := f.call("PUT", path, body, key, f.requester.cookie, nil)
		var policy useroperations.Policy
		assertUserJSON(t, w, 200, &policy)
		if policy.Version != 1 || policy.ExpiresAt == nil || policy.QuotaBytes != 4096 || w.Header().Get("ETag") != `"revision-1"` || w.Header().Get("Idempotency-Replayed") != "" {
			t.Fatal("policy", w.Header(), policy)
		}
		var got useroperations.Policy
		assertUserJSON(t, f.call("GET", path, "", "", f.reader.cookie, nil), 200, &got)
		if !reflect.DeepEqual(policy, got) {
			t.Fatal("policy read", got)
		}
		after := f.counts()
		if after != ([7]int{before[0], before[1], before[2] + 1, before[3] + 1, before[4], before[5], before[6]}) {
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
	t.Run("enable-per-item-readers-and-revocation", func(t *testing.T) {
		f := f
		f.t = t
		items := []useroperations.BatchItemRequest{{NodeID: f.node, Username: "alice", Action: "enable", ExpectedVersion: 2}, {NodeID: f.sibling, Username: "alice", Action: "enable", ExpectedVersion: 1}, {NodeID: f.node, Username: "missing", Action: "enable", ExpectedVersion: 1}}
		body, key := userBatchBody(t, items), uuid.NewString()
		before := f.counts()
		missingNode := append([]useroperations.BatchItemRequest(nil), items...)
		missingNode[0].NodeID = uuid.Must(uuid.NewV7())
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", userBatchBody(t, missingNode), key, f.requester.cookie, nil), 400, "invalid-request")
		f.unchanged(before)
		w := f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, nil)
		var batch useroperations.Batch
		assertUserJSON(t, w, 202, &batch)
		if len(batch.Items) != 3 || batch.Items[0].State != "queued" || batch.Items[1].State != "forbidden" || batch.Items[1].ErrorType != "forbidden" || batch.Items[2].State != "failed" || batch.Items[2].ErrorType != "not_found" {
			t.Fatal("per-item results", batch)
		}
		stored, err := f.service.GetBatch(t.Context(), batch.ID)
		if err != nil || !reflect.DeepEqual(stored.Items, batch.Items) || stored.ActorIdentityID == nil || *stored.ActorIdentityID != f.requester.principal.IdentityID {
			t.Fatal("stored per-item results", stored, err)
		}
		after := f.counts()
		if after != ([7]int{before[0] + 1, before[1] + 3, before[2] + 1, before[3], before[4], before[5], before[6]}) {
			t.Fatal("enable intent", before, after)
		}
		w = f.call("POST", "/api/v1/user-batches", body, key, f.requester.cookie, nil)
		var replay useroperations.Batch
		assertUserJSON(t, w, 202, &replay)
		if replay.ID != batch.ID || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("enable replay", replay, w.Header())
		}
		f.unchanged(after)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", userBatchBody(t, items[:1]), key, f.requester.cookie, nil), 409, "idempotency-conflict")
		f.unchanged(after)
		path := "/api/v1/user-batches/" + batch.ID.String()
		assertUserJSON(t, f.call("GET", path, "", "", f.requester.cookie, nil), 200, nil)
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 403, "forbidden")
		assertApplyHTTPProblem(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.requester.cookie, nil), 403, "forbidden")
		f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'Viewer','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'Viewer','workspace',?)`, uuid.Must(uuid.NewV7()), f.reader.principal.IdentityID, f.workspace, value.Timestamp{Valid: true})
		assertUserJSON(t, f.call("GET", path, "", "", f.reader.cookie, nil), 200, nil)
		assertUserJSON(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.reader.cookie, nil), 200, nil)
		f.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2`, `DELETE FROM role_bindings WHERE identity_id=? AND workspace_id=?`, f.requester.principal.IdentityID, f.workspace)
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 403, "forbidden")
		f.bind(f.requester, f.node, "UserManager")
		f.exec(`UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, `UPDATE auth_sessions SET revoked_at=? WHERE id=?`, value.Timestamp{Valid: true}, f.requester.principal.SessionID)
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 401, "unauthenticated")
		f.unchanged(after)
	})
}

func TestUserOperationsHTTPAssemblyBackendIntegration(t *testing.T) {
	f := newUserOperationsHTTPFixture(t)
	f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'Viewer','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'Viewer','workspace',?)`, uuid.Must(uuid.NewV7()), f.reader.principal.IdentityID, f.workspace, value.Timestamp{Valid: true})
	path := "/api/v1/nodes/" + f.node.String() + "/users/alice/policy"
	body := userBatchBody(t, []useroperations.BatchItemRequest{{NodeID: f.node, Username: "alice", Action: "enable", ExpectedVersion: 1}})
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%v", enabled), func(t *testing.T) {
			f := f
			f.t = t
			before := f.counts()
			authorization := Authorization{Authentication: f.s.auth, RBAC: f.s.rbac, Approvals: f.s.approvals, Audit: f.s.audit}
			f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{}, Authorization{})
			assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", "{", "", f.requester.cookie, nil), 401, "unauthenticated")
			modules := Modules{}
			if enabled {
				modules.UserOperations = f.service
			}
			f.s = newTestServer(t, testHTTPConfig(false), f.b, modules, authorization)
			if !enabled {
				assertApplyHTTPProblem(t, f.call("GET", "/api/v1/user-operations/metrics", "", "", f.reader.cookie, nil), 503, "service-unavailable")
				for _, route := range []struct{ method, path string }{{"GET", path}, {"PUT", path}, {"POST", "/api/v1/user-batches"}, {"GET", "/api/v1/user-batches/" + uuid.Must(uuid.NewV7()).String()}} {
					assertApplyHTTPProblem(t, f.call(route.method, route.path, "{", "", f.requester.cookie, nil), 503, "service-unavailable")
				}
				assertApplyHTTPProblem(t, f.call("PUT", path, "{", "", f.reader.cookie, nil), 403, "forbidden")
				assertApplyHTTPProblem(t, f.call("GET", path, "", "", nil, nil), 401, "unauthenticated")
				assertApplyHTTPProblem(t, f.call("GET", "/api/v1/nodes/bad/users/alice/policy", "", "", f.requester.cookie, nil), 404, "not-found")
				f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{UserOperations: f.service}, authorization)
			}
			f.unchanged(before)
			var batch useroperations.Batch
			assertUserJSON(t, f.call("POST", "/api/v1/user-batches", body, uuid.NewString(), f.requester.cookie, nil), 202, &batch)
			assertUserJSON(t, f.call("GET", "/api/v1/user-batches/"+batch.ID.String(), "", "", f.requester.cookie, nil), 200, nil)
			var nilService *useroperations.Service
			f.s = newTestServer(t, testHTTPConfig(false), f.b, Modules{UserOperations: nilService}, authorization)
			assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 503, "service-unavailable")
			var nilRBAC *rbac.Service
			authorization.RBAC = nilRBAC
			// Development bypasses the entry guard but still needs a real batch
			// authorizer. This detects a stale or typed-nil module capability.
			f.s = newTestServer(t, testHTTPConfig(true), f.b, Modules{UserOperations: f.service}, authorization)
			assertApplyHTTPProblem(t, f.call("POST", "/api/v1/user-batches", "{", "", nil, nil), 503, "service-unavailable")
		})
	}
}
