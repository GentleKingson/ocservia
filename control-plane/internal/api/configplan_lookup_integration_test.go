package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

type observedConfigPlanLookup struct {
	configPlanLookup
	gets, resources []uuid.UUID
	getErr          error
	result          *configplan.Plan
}

func (l *observedConfigPlanLookup) Get(ctx context.Context, id uuid.UUID) (configplan.Plan, error) {
	l.gets = append(l.gets, id)
	if l.getErr != nil {
		return configplan.Plan{}, l.getErr
	}
	if l.result != nil {
		return *l.result, nil
	}
	return l.configPlanLookup.Get(ctx, id)
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
	if !reflect.DeepEqual(lookup.resources, []uuid.UUID{plan.ID}) || len(lookup.gets) != 0 {
		t.Fatalf("Plan guard must use Resource only: %+v", lookup)
	}
	approval := f.approval(plan, false)
	if !reflect.DeepEqual(lookup.gets, []uuid.UUID{plan.ID}) {
		t.Fatalf("approval must read interpreted Plan: %+v", lookup)
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
			name   string
			change func(*configplan.Plan)
			status int
			kind   string
		}{
			{"pending", func(p *configplan.Plan) { p.Validation = "pending" }, 409, "config-plan-not-ready"},
			{"expired", func(p *configplan.Plan) { p.ExpiresAt = value.Timestamp{Valid: true} }, 409, "config-plan-not-ready"},
			{"no-expiry", func(p *configplan.Plan) { p.ExpiresAt = value.Timestamp{} }, 409, "config-plan-not-ready"},
			{"foreign-workspace", func(p *configplan.Plan) { p.WorkspaceID = uuid.Must(uuid.NewV7()) }, 400, "invalid-request"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				copy := plan
				tc.change(&copy)
				lookup.result = &copy
				assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), tc.status, tc.kind)
			})
		}
		lookup.result = nil
		lookup.getErr = database.ErrNotFound
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), 409, "config-plan-not-ready")
		lookup.getErr = nil
		f.s.configPlanLookup = nil
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("GET", approvalPath, "", "", f.approver.cookie, nil), 404, "not-found")
		assertApplyHTTPProblem(t, f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil), 400, "invalid-request")
		f.s.configPlanLookup = lookup
	})
}
