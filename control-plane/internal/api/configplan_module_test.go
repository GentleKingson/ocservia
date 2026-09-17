package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	started  chan context.Context
	canceled chan error
	release  chan struct{}
}

func (p *blockedConfigPlans) wait(ctx context.Context) error {
	p.started <- ctx
	<-ctx.Done()
	p.canceled <- ctx.Err()
	<-p.release
	return ctx.Err()
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
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", `{"approval_id":"` + baselineID + `"}`},
	} {
		for _, cancelRequest := range []bool{false, true} {
			name := "deadline"
			if cancelRequest {
				name = "cancellation"
			}
			t.Run(route.method+route.path+"/"+name, func(t *testing.T) {
				timeout := 30 * time.Millisecond
				if cancelRequest {
					timeout = time.Second
				}
				plans := &blockedConfigPlans{started: make(chan context.Context, 1), canceled: make(chan error, 1), release: make(chan struct{})}
				config := testHTTPConfig(true)
				config.BodyLimit, config.RequestTimeout = 1024, timeout
				s := newTestServer(t, config, nil, Modules{ConfigPlans: plans}, Authorization{})
				var once sync.Once
				release := func() { once.Do(func() { close(plans.release) }) }
				t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := baselineRequest(route.method, route.path, strings.NewReader(route.body)).WithContext(ctx)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "context-test")
					w := httptest.NewRecorder()
					s.http.Handler.ServeHTTP(w, r)
					done <- w
				}()
				select {
				case got := <-plans.started:
					if _, ok := got.Deadline(); !ok {
						t.Fatal("HTTP deadline lost")
					}
					if got.Value(requestIDKey{}) != "baseline-request" {
						t.Fatal("request context lost")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("business method did not start")
				}
				wantErr := context.DeadlineExceeded
				if cancelRequest {
					cancel()
					wantErr = context.Canceled
				}
				select {
				case got := <-plans.canceled:
					if !errors.Is(got, wantErr) {
						t.Fatalf("service context: %v", got)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("service did not observe cancellation")
				}
				select {
				case w := <-done:
					if w.Code != 503 {
						t.Fatal("timeout status", w.Code)
					}
				case <-time.After(time.Second):
					t.Fatal("outer HTTP handler did not return")
				}
				shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancelShutdown()
				if err := s.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown forgot ConfigPlan request: %v", err)
				}
				release()
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
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
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", "{}", "key", 400, "invalid-id"},
		{"POST", "/api/v1/config-plans/" + baselineID + "/apply", `{"approval_id":"` + baselineID + `","reason":"apply"}`, "key", 503, "service-unavailable"},
	} {
		r := baselineRequest(route.method, route.path, strings.NewReader(route.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", route.key)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		assertApplyHTTPProblem(t, w, route.status, route.kind)
	}
}
