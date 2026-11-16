package api

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/google/uuid"
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
	f.s.configPlanLookup = nil
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 404, "not-found")
}
