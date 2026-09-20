package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type observedConfigPlanLookup struct {
	configPlanLookup
	bindings, resources []uuid.UUID
	bindingErr          error
	result              *configplan.ApprovalBinding
}

func (l *observedConfigPlanLookup) ApprovalBinding(ctx context.Context, id uuid.UUID) (configplan.ApprovalBinding, error) {
	l.bindings = append(l.bindings, id)
	if l.bindingErr != nil {
		return configplan.ApprovalBinding{}, l.bindingErr
	}
	if l.result != nil {
		return *l.result, nil
	}
	return l.configPlanLookup.ApprovalBinding(ctx, id)
}

func (l *observedConfigPlanLookup) Resource(ctx context.Context, id uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	l.resources = append(l.resources, id)
	return l.configPlanLookup.Resource(ctx, id)
}

func TestPlanRoutesBackendHTTPIntegration(t *testing.T) {
	// Only resource-scoped routes share this database; maintenance suites stay isolated.
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, applyHTTPFixture)
	}{
		{"TestConfigPlanLookupBackendHTTPIntegration", testConfigPlanLookupBackendHTTPIntegration},
		{"TestConfigPlanModuleBackendHTTPIntegration", testConfigPlanModuleBackendHTTPIntegration},
		{"TestConfigRevisionBackendHTTPIntegration", testConfigRevisionBackendHTTPIntegration},
		{"TestBusinessRouteActionsBackendHTTPIntegration", testBusinessRouteActionsBackendHTTPIntegration},
	} {
		if !t.Run(scenario.name, func(t *testing.T) {
			scenario.run(t, newApplyHTTPFixtureWithBackend(t, b, owner))
		}) {
			return
		}
	}
}

func testConfigPlanLookupBackendHTTPIntegration(t *testing.T, f applyHTTPFixture) {
	assertStatus := func(t *testing.T, w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("want %d, got %d %s", status, w.Code, w.Body)
		}
	}
	plan := f.plan(true)
	lookup := &observedConfigPlanLookup{configPlanLookup: f.s.configPlanLookup}
	f.s.configPlanLookup = lookup
	path := "/api/v1/config-plans/" + plan.ID.String()
	assertStatus(t, f.call("GET", path, "", "", f.reader.cookie, nil), 200)
	if !reflect.DeepEqual(lookup.resources, []uuid.UUID{plan.ID}) || len(lookup.bindings) != 0 {
		t.Fatalf("Plan guard must use Resource only: %+v", lookup)
	}
	approval := f.approval(plan, false)
	if !reflect.DeepEqual(lookup.bindings, []uuid.UUID{plan.ID}) {
		t.Fatalf("approval must request domain binding: %+v", lookup)
	}
	wantSummary, err := json.Marshal(map[string]any{"node_id": plan.NodeID, "expected_revision": plan.ExpectedRevision, "candidate_hash": plan.CandidateHash, "current_hash": plan.CurrentHash, "diff_redacted": plan.DiffRedacted, "expires_at": plan.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if json.Unmarshal(approval.ConfigPlanSummary, &got) != nil || json.Unmarshal(wantSummary, &want) != nil || !reflect.DeepEqual(got, want) || approval.RequestHash != plan.CandidateHash || len(approval.RequestSummary) != 0 {
		t.Fatalf("approval binding changed: %+v", approval)
	}
	scopes, err := f.s.approvals.AuthorityResources(t.Context(), approval.ID)
	if err != nil || !reflect.DeepEqual(scopes, []approvals.AuthorityResource{{WorkspaceID: f.workspace, Type: "node", ID: plan.NodeID}}) {
		t.Fatalf("approval scopes: %+v %v", scopes, err)
	}
	approvalPath := "/api/v1/approval-requests/" + approval.ID.String()
	t.Run("interpreted-plan-checks", func(t *testing.T) {
		f := f
		f.t = t
		f.s.configPlanLookup = lookup.configPlanLookup
		t.Cleanup(func() { f.s.configPlanLookup = lookup })
		body := fmt.Sprintf(`{"action":"config.apply","resource_type":"config_plan","resource_id":%q,"reason":"review","ttl_seconds":900}`, plan.ID)
		var result []byte
		if err := f.row(`SELECT result FROM agent_command_results WHERE command_id=(SELECT command_id FROM operations WHERE id=$1)`, `SELECT result FROM agent_command_results WHERE command_id=(SELECT command_id FROM operations WHERE id=?)`, plan.OperationID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, state string
			change      func(*agentv1.ConfigPlanResult)
		}{
			{name: "pending", state: "queued"},
			{name: "failed", state: "failed"},
			{name: "unknown", state: "unknown"},
			{name: "expired-state", state: "expired"},
			{name: "candidate-hash", change: func(r *agentv1.ConfigPlanResult) { r.CandidateHash = bytes.Repeat([]byte{0x55}, 32) }},
			{name: "unsafe-diff", change: func(r *agentv1.ConfigPlanResult) { r.DiffRedacted += "private-key-material" }},
			{name: "unsafe-warning", change: func(r *agentv1.ConfigPlanResult) { r.Warnings = []string{"private-key-material"} }},
			{name: "current-changed", change: func(r *agentv1.ConfigPlanResult) { r.CurrentUnchanged = false }},
			{name: "staging-not-clean", change: func(r *agentv1.ConfigPlanResult) { r.StagingCleaned = false }},
			{name: "missing-current-hash", change: func(r *agentv1.ConfigPlanResult) { r.CurrentHash = nil }},
			{name: "expired-time"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := f
				g.t = t
				state, payload, expires := "succeeded", result, plan.ExpiresAt
				if tc.state != "" {
					state = tc.state
				}
				if tc.name == "expired-time" {
					expires = value.Timestamp{Valid: true}
				}
				if tc.change != nil {
					var validation agentv1.ConfigPlanResult
					if err := proto.Unmarshal(result, &validation); err != nil {
						t.Fatal(err)
					}
					tc.change(&validation)
					var err error
					payload, err = proto.Marshal(&validation)
					if err != nil {
						t.Fatal(err)
					}
				}
				set := func(state string, payload []byte, expires value.Timestamp) {
					g.exec(`UPDATE operations SET state=$1 WHERE id=$2`, `UPDATE operations SET state=? WHERE id=?`, state, plan.OperationID)
					g.exec(`UPDATE agent_command_results SET result=$1 WHERE command_id=(SELECT command_id FROM operations WHERE id=$2)`, `UPDATE agent_command_results SET result=? WHERE command_id=(SELECT command_id FROM operations WHERE id=?)`, payload, plan.OperationID)
					g.exec(`UPDATE config_plans SET expires_at=$1 WHERE id=$2`, `UPDATE config_plans SET expires_at=? WHERE id=?`, expires, plan.ID)
				}
				t.Cleanup(func() { set("succeeded", result, plan.ExpiresAt) })
				// Owner-only fault injection; the request reads through the real runtime.
				set(state, payload, expires)
				w := g.call("POST", "/api/v1/approval-requests", body, "", g.requester.cookie, nil)
				assertApplyHTTPProblem(t, w, 409, "config-plan-not-ready")
				var p struct {
					Detail string `json:"detail"`
				}
				if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Detail != "the plan must be valid and unexpired before approval" {
					t.Fatal(w.Body)
				}
				if bytes.Contains(w.Body.Bytes(), []byte("private-key-material")) {
					t.Fatal("unsafe validation content leaked")
				}
			})
		}
		var count int
		if err := f.row(`SELECT count(*) FROM approval_requests WHERE resource_id=$1`, `SELECT count(*) FROM approval_requests WHERE resource_id=?`, plan.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("rejected preparation persisted approvals: %d %v", count, err)
		}
		// A reviewer can see this workspace but cannot request config.apply.
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.reader.cookie, nil), 403, "forbidden")
		foreign := f
		foreign.workspace = uuid.Must(uuid.NewV7())
		foreign.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'foreign approval',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'foreign approval',?,?,?)`, foreign.workspace, foreign.workspace.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
		foreignPlan := foreign.plan(true)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", foreign.workspace.String()) }), 400, "invalid-request")
		foreign.exec(`UPDATE config_plans SET expires_at=$1 WHERE id=$2`, `UPDATE config_plans SET expires_at=? WHERE id=?`, value.Timestamp{Valid: true}, foreignPlan.ID)
		foreignBody := fmt.Sprintf(`{"action":"config.apply","resource_type":"config_plan","resource_id":%q,"reason":"review","ttl_seconds":900}`, foreignPlan.ID)
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", foreignBody, "", f.requester.cookie, nil), 409, "config-plan-not-ready")
	})
	t.Run("authority-resources-before-legacy-fallback", func(t *testing.T) {
		assertStatus(t, f.call("GET", approvalPath, "", "", f.approver.cookie, nil), 200)
		if len(lookup.resources) != 1 {
			t.Fatal("saved scopes unexpectedly used legacy Plan lookup")
		}
		// A second saved authority must also be authorized, not just the Plan node.
		second := uuid.Must(uuid.NewV7())
		f.exec(`INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES($1,$2,'node',$3)`, `INSERT INTO approval_authority_resources(approval_id,workspace_id,resource_type,resource_id) VALUES(?,?,'node',?)`, approval.ID, f.workspace, second)
		assertApplyHTTPProblem(t, f.call("GET", approvalPath, "", "", f.approver.cookie, nil), 403, "forbidden")
		if len(lookup.resources) != 1 {
			t.Fatal("authority denial fell back to Plan")
		}
		// Owner-only fixture conversion to a legacy approval without saved scopes.
		f.exec(`DELETE FROM approval_authority_resources WHERE approval_id=$1`, `DELETE FROM approval_authority_resources WHERE approval_id=?`, approval.ID)
		assertStatus(t, f.call("GET", approvalPath, "", "", f.approver.cookie, nil), 200)
		if !reflect.DeepEqual(lookup.resources, []uuid.UUID{plan.ID, plan.ID}) {
			t.Fatal("legacy approval did not resolve Plan ownership", lookup.resources)
		}
	})
	t.Run("approval-plan-checks", func(t *testing.T) {
		body := fmt.Sprintf(`{"action":"config.apply","resource_type":"config_plan","resource_id":%q,"reason":"review","ttl_seconds":900}`, plan.ID)
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"not-ready", configplan.ErrApprovalNotReady},
			{"missing-plan", database.ErrNotFound},
			{"read-error", database.ErrPermission},
		} {
			t.Run(tc.name, func(t *testing.T) {
				lookup.bindingErr = tc.err
				assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), 409, "config-plan-not-ready")
			})
		}
		lookup.bindingErr = nil
		lookup.result = &configplan.ApprovalBinding{WorkspaceID: uuid.Must(uuid.NewV7())}
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), 400, "invalid-request")
		lookup.result = nil
		f.s.configPlanLookup = nil
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("GET", approvalPath, "", "", f.approver.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), 400, "invalid-request")
		f.s.configPlanLookup = lookup
	})
}
