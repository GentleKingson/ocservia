package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

type observedConfigPlans struct {
	ConfigPlans
	creates, gets, applies atomic.Int64
}

func (p *observedConfigPlans) Create(ctx context.Context, r configplan.CreateRequest) (configplan.Plan, bool, error) {
	p.creates.Add(1)
	return p.ConfigPlans.Create(ctx, r)
}
func (p *observedConfigPlans) Get(ctx context.Context, id uuid.UUID) (configplan.Plan, error) {
	p.gets.Add(1)
	return p.ConfigPlans.Get(ctx, id)
}
func (p *observedConfigPlans) Apply(ctx context.Context, r configplan.ApplyRequest) (operations.Operation, bool, error) {
	p.applies.Add(1)
	return p.ConfigPlans.Apply(ctx, r)
}

type blockedConfigPlans struct {
	ConfigPlans
	*blockedRequest
}

func (p *blockedConfigPlans) Create(ctx context.Context, _ configplan.CreateRequest) (configplan.Plan, bool, error) {
	return configplan.Plan{}, false, p.wait(ctx)
}
func (p *blockedConfigPlans) Get(ctx context.Context, _ uuid.UUID) (configplan.Plan, error) {
	return configplan.Plan{}, p.wait(ctx)
}
func (p *blockedConfigPlans) Apply(ctx context.Context, _ configplan.ApplyRequest) (operations.Operation, bool, error) {
	return operations.Operation{}, false, p.wait(ctx)
}

func TestConfigPlanHTTPContextLifetime(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/v1/nodes/" + baselineID + "/config-plans", `{}`},
		{"GET", "/api/v1/config-plans/" + baselineID, ""},
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", `{"reason":"context test"}`},
	} {
		checkRequestLifetime(t, route.method+route.path+"/", lifetimeJSONRequest(route.method, route.path, route.body), func(b *blockedRequest) Modules {
			return Modules{ConfigPlans: &blockedConfigPlans{blockedRequest: b}}
		}, Authorization{})
	}
}

func TestConfigPlanHTTPDisabledDevelopment(t *testing.T) {
	s := baselineServer(t, true)
	for _, route := range []struct {
		method, path, body, key string
		status                  int
		kind                    string
	}{
		{"POST", "/api/v1/nodes/bad/config-plans", "{", "", 503, "service-unavailable"},
		{"GET", "/api/v1/config-plans/bad", "", "", 400, "invalid-id"},
		{"GET", "/api/v1/config-plans/" + baselineID, "", "", 503, "service-unavailable"},
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", "{", "", 400, "idempotency-key-required"},
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", "{}", "key", 503, "service-unavailable"},
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", `{"reason":"apply"}`, "key", 503, "service-unavailable"},
	} {
		r := baselineRequest(route.method, route.path, strings.NewReader(route.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", route.key)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		assertApplyHTTPProblem(t, w, route.status, route.kind)
	}
}
