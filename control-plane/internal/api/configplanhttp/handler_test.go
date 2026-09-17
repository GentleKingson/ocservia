package configplanhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
)

const testID = "019fc0a4-6d92-765c-a8a1-4af556614cc3"
const createPath = "/api/v1/nodes/" + testID + "/config-plans"
const getPath = "/api/v1/config-plans/" + testID
const applyPath = getPath + "/apply"
const createBody = `{"expected_revision":7,"template":{"name":"unit","directives":[{"name":"tcp-port","value":"${port}"}]},"node_variables":{"port":"443"},"ttl_seconds":900,"reason":"review"}`
const applyBody = `{"approval_id":"` + testID + `","reason":"apply"}`

type fakePlans struct {
	create func(context.Context, configplan.CreateRequest) (configplan.Plan, bool, error)
	get    func(context.Context, uuid.UUID) (configplan.Plan, error)
	apply  func(context.Context, configplan.ApplyRequest) (operations.Operation, bool, error)
}

func (f fakePlans) Create(ctx context.Context, r configplan.CreateRequest) (configplan.Plan, bool, error) {
	return f.create(ctx, r)
}
func (f fakePlans) Get(ctx context.Context, id uuid.UUID) (configplan.Plan, error) {
	return f.get(ctx, id)
}
func (f fakePlans) Apply(ctx context.Context, r configplan.ApplyRequest) (operations.Operation, bool, error) {
	return f.apply(ctx, r)
}

var testInfo = RequestInfo{ActorID: "authenticated-actor", ActorIdentityID: uuid.MustParse(testID), ActorSessionID: uuid.MustParse("019fc0a4-6d92-765c-a8a1-4af556614cc4"), RequestID: "authenticated-request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}

func testMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux, func(_ string, next http.HandlerFunc) http.HandlerFunc { return next })
	return mux
}

func request(method, path, body, key, media string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", media)
	r.Header.Set("Idempotency-Key", key)
	for _, name := range []string{"X-Actor-ID", "X-Identity-ID", "X-Session-ID", "X-Workspace-ID", "X-Request-ID", "Traceparent"} {
		r.Header.Set(name, "untrusted")
	}
	return r
}

func serve(mux http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func problem(t *testing.T, w *httptest.ResponseRecorder, status int, kind, detail string) {
	t.Helper()
	var p struct {
		Type, Detail, Title string
		Status              int
	}
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || w.Code != status || p.Status != status || p.Type != "https://ocservia.dev/problems/"+kind || (detail != "" && p.Detail != detail) || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("want %d %s %q; got %d %s", status, kind, detail, w.Code, w.Body)
	}
	if w.Header().Get("Location") != "" || w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("error has success headers", w.Header())
	}
}

func TestConfigPlanHTTPResponses(t *testing.T) {
	id := uuid.MustParse(testID)
	plan := configplan.Plan{ID: id, NodeID: id, OperationID: id, TemplateName: "unit", ExpectedRevision: 7, State: "queued", Validation: "pending", Warnings: []json.RawMessage{json.RawMessage(`"warning"`)}, ExpiresAt: value.Timestamp{Valid: true}, CreatedAt: value.Timestamp{Valid: true}}
	op := operations.Operation{ID: testID, State: "queued"}
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			calls := []string{}
			h := New(fakePlans{
				create: func(_ context.Context, got configplan.CreateRequest) (configplan.Plan, bool, error) {
					calls = append(calls, "create")
					want := configplan.CreateRequest{NodeID: id, ExpectedRevision: 7, Template: configplan.Template{Name: "unit", Directives: []configplan.Directive{{Name: "tcp-port", Value: "${port}"}}}, NodeVariables: map[string]string{"port": "443"}, TTL: 15 * time.Minute, IdempotencyKey: "key", ActorID: testInfo.ActorID, ActorIdentityID: testInfo.ActorIdentityID, ActorSessionID: testInfo.ActorSessionID, RequestID: testInfo.RequestID, Traceparent: testInfo.Traceparent, Reason: "review"}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("Create: %+v, want %+v", got, want)
					}
					return plan, replay, nil
				},
				get: func(_ context.Context, got uuid.UUID) (configplan.Plan, error) {
					calls = append(calls, "get")
					if got != id {
						t.Error("Get ID", got)
					}
					return plan, nil
				},
				apply: func(_ context.Context, got configplan.ApplyRequest) (operations.Operation, bool, error) {
					calls = append(calls, "apply")
					want := configplan.ApplyRequest{PlanID: id, ApprovalID: id, IdempotencyKey: "key", ActorID: testInfo.ActorID, ActorIdentityID: testInfo.ActorIdentityID, ActorSessionID: testInfo.ActorSessionID, RequestID: testInfo.RequestID, Traceparent: testInfo.Traceparent, Reason: "apply"}
					if got != want {
						t.Errorf("Apply: %+v, want %+v", got, want)
					}
					return op, replay, nil
				},
			}, func(*http.Request) RequestInfo { return testInfo }, nil)
			mux := testMux(h)
			for _, tc := range []struct {
				method, path, body, location string
				status                       int
				result                       any
			}{
				{"POST", createPath, createBody, getPath, 202, plan},
				{"GET", getPath, "", "", 200, plan},
				{"POST", applyPath, applyBody, "/api/v1/operations/" + testID, 202, op},
			} {
				w := serve(mux, request(tc.method, tc.path, tc.body, " key \t", "application/json; charset=utf-8"))
				want, err := json.Marshal(tc.result)
				if err != nil {
					t.Fatal(err)
				}
				if w.Code != tc.status || w.Body.String() != string(want)+"\n" || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Location") != tc.location {
					t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body)
				}
				flag := ""
				if replay && tc.method == "POST" {
					flag = "true"
				}
				if w.Header().Get("Idempotency-Replayed") != flag {
					t.Fatal(w.Header())
				}
			}
			if !reflect.DeepEqual(calls, []string{"create", "get", "apply"}) {
				t.Fatal(calls)
			}
		})
	}
}

func TestConfigPlanHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		err          error
		status       int
		kind, detail string
	}{
		{configplan.ErrInvalid, 400, "config-plan-invalid", "the template, variables, revision, or lifetime is invalid"},
		{operations.ErrInvalidRequest, 400, "config-plan-invalid", "the template, variables, revision, or lifetime is invalid"},
		{configplan.ErrStaleRevision, 409, "stale-revision", "refresh configuration state and plan again"},
		{approvals.ErrNotReady, 409, "approval-not-ready", "an unexpired independent approval for this exact plan is required"},
		{configplan.ErrCapability, 409, "capability-unavailable", "the node cannot validate this configuration"},
		{operations.ErrCapabilityMissing, 409, "capability-unavailable", "the node cannot validate this configuration"},
		{operations.ErrIdempotencyConflict, 409, "idempotency-conflict", "the idempotency key was used for different configuration content"},
		{operations.ErrConfigApplyActive, 409, "config-apply-active", "wait for the active apply or its reconciliation to reach a terminal state"},
		{database.ErrNotFound, 404, "not-found", "the configuration plan or node does not exist"},
		{errors.New("private database details"), 503, "config-plan-unavailable", "configuration planning is temporarily unavailable"},
		{errors.Join(database.ErrNotFound, configplan.ErrInvalid), 400, "config-plan-invalid", "the template, variables, revision, or lifetime is invalid"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", tc.err)
			h := New(fakePlans{
				create: func(context.Context, configplan.CreateRequest) (configplan.Plan, bool, error) {
					return configplan.Plan{}, true, err
				},
				get: func(context.Context, uuid.UUID) (configplan.Plan, error) { return configplan.Plan{}, err },
				apply: func(context.Context, configplan.ApplyRequest) (operations.Operation, bool, error) {
					return operations.Operation{}, true, err
				},
			}, func(*http.Request) RequestInfo { return testInfo }, nil)
			mux := testMux(h)
			for _, r := range []*http.Request{request("POST", createPath, createBody, "key", "application/json"), request("GET", getPath, "", "", ""), request("POST", applyPath, applyBody, "key", "application/json")} {
				problem(t, serve(mux, r), tc.status, tc.kind, tc.detail)
			}
		})
	}
}

func TestConfigPlanHTTPValidationOrder(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, key, media string
		enabled                              bool
		status                               int
		kind, detail                         string
	}{
		{"create-disabled-before-id", "POST", strings.Replace(createPath, testID, "bad", 1), "{", "", "", false, 503, "service-unavailable", "configuration plan service is unavailable"},
		{"create-id", "POST", strings.Replace(createPath, testID, "bad", 1), "{", "", "", true, 400, "invalid-id", "node_id must be UUIDv7"},
		{"create-key", "POST", createPath, "{", " \t ", "", true, 400, "idempotency-key-required", "Idempotency-Key must be provided"},
		{"create-media", "POST", createPath, "{", "key", "text/plain", true, 415, "unsupported-media-type", "Content-Type must be application/json"},
		{"create-json", "POST", createPath, "{", "key", "application/json", true, 400, "invalid-request", "request body is invalid"},
		{"create-identity-body", "POST", createPath, `{"actor_id":"forged"}`, "key", "application/json", true, 400, "invalid-request", "request body is invalid"},
		{"get-id-before-disabled", "GET", "/api/v1/config-plans/bad", "", "", "", false, 400, "invalid-id", "plan_id must be UUIDv7"},
		{"get-disabled", "GET", getPath, "", "", "", false, 503, "service-unavailable", "configuration plan service is unavailable"},
		{"apply-id-before-disabled", "POST", "/api/v1/config-plans/bad/apply", "{", "", "", false, 400, "invalid-id", "plan_id must be UUIDv7"},
		{"apply-key-before-disabled", "POST", applyPath, "{", "", "", false, 400, "idempotency-key-required", "Idempotency-Key must be provided"},
		{"apply-media-before-disabled", "POST", applyPath, "{", "key", "", false, 415, "unsupported-media-type", "Content-Type must be application/json"},
		{"apply-json-before-disabled", "POST", applyPath, "{", "key", "application/json", false, 400, "invalid-request", "request body is invalid"},
		{"apply-approval-before-disabled", "POST", applyPath, `{}`, "key", "application/json", false, 400, "invalid-id", "approval_id must be UUIDv7"},
		{"apply-disabled", "POST", applyPath, applyBody, "key", "application/json", false, 503, "service-unavailable", "configuration plan service is unavailable"},
		{"apply-identity-body", "POST", applyPath, `{"session_id":"forged"}`, "key", "application/json", true, 400, "invalid-request", "request body is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var plans Plans
			if tc.enabled {
				plans = fakePlans{}
			}
			h := New(plans, func(*http.Request) RequestInfo { t.Fatal("validation reached identity"); return RequestInfo{} }, nil)
			problem(t, serve(testMux(h), request(tc.method, tc.path, tc.body, tc.key, tc.media)), tc.status, tc.kind, tc.detail)
		})
	}
	for _, body := range []string{"{\"reason\":\"\xff\"}", `{} {}`, `{"unknown":1}`} {
		for _, path := range []string{createPath, applyPath} {
			h := New(fakePlans{}, func(*http.Request) RequestInfo { t.Fatal("invalid JSON reached identity"); return RequestInfo{} }, nil)
			problem(t, serve(testMux(h), request("POST", path, body, "key", "application/json")), 400, "invalid-request", "")
		}
	}
}

func TestConfigPlanHTTPSecretOrder(t *testing.T) {
	first, second := uuid.MustParse(testID), uuid.MustParse("019fc0a4-6d92-765c-a8a1-4af556614cc5")
	body := fmt.Sprintf(`{"template":{"directives":[{"name":"server-key","secret_ref":{"secret_ref_id":%q}},{"name":"tcp-port","value":"443"},{"name":"server-cert","secret_ref":{"secret_ref_id":%q}}]}}`, first, second)
	for _, failAt := range []int{-1, 0, 1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			var checked []uuid.UUID
			created := 0
			var check SecretUse
			if failAt != -1 {
				check = func(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
					checked = append(checked, id)
					if len(checked) == failAt {
						httpx.WriteProblem(w, r, 403, "https://ocservia.dev/problems/forbidden", "Access denied", "denied")
						return false
					}
					return true
				}
			}
			h := New(fakePlans{create: func(_ context.Context, got configplan.CreateRequest) (configplan.Plan, bool, error) {
				created++
				if len(got.Template.Directives) != 3 || got.Template.Directives[0].SecretRef.ID != first || got.Template.Directives[2].SecretRef.ID != second {
					t.Fatal("directive order changed")
				}
				return configplan.Plan{ID: first}, false, nil
			}}, func(*http.Request) RequestInfo { return testInfo }, check)
			w := serve(testMux(h), request("POST", createPath, body, "key", "application/json"))
			switch failAt {
			case -1:
				problem(t, w, 503, "service-unavailable", "secret reference service is unavailable")
			case 0:
				if w.Code != 202 || created != 1 || !reflect.DeepEqual(checked, []uuid.UUID{first, second}) {
					t.Fatalf("authorized secrets: %d %v %d", w.Code, checked, created)
				}
			default:
				problem(t, w, 403, "forbidden", "denied")
				if !reflect.DeepEqual(checked, []uuid.UUID{first, second}[:failAt]) {
					t.Fatal("continued after denied reference", checked)
				}
			}
			if failAt != 0 && created != 0 {
				t.Fatal("denial submitted Plan")
			}
		})
	}
}

func TestConfigPlanHTTPContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), struct{}{}, "marker"), time.Now().Add(time.Minute))
	defer cancel()
	info := func(r *http.Request) RequestInfo {
		if r.Context() != ctx {
			t.Error("identity context replaced")
		}
		return testInfo
	}
	check := func(got context.Context) {
		if got != ctx || got.Value(struct{}{}) != "marker" {
			t.Error("service context replaced")
		}
		deadline, ok := got.Deadline()
		want, _ := ctx.Deadline()
		if !ok || deadline != want {
			t.Error("deadline changed")
		}
		if !errors.Is(got.Err(), context.Canceled) {
			t.Error("cancellation lost")
		}
	}
	h := New(fakePlans{
		create: func(c context.Context, _ configplan.CreateRequest) (configplan.Plan, bool, error) {
			check(c)
			return configplan.Plan{}, false, c.Err()
		},
		get: func(c context.Context, _ uuid.UUID) (configplan.Plan, error) {
			check(c)
			return configplan.Plan{}, c.Err()
		},
		apply: func(c context.Context, _ configplan.ApplyRequest) (operations.Operation, bool, error) {
			check(c)
			return operations.Operation{}, false, c.Err()
		},
	}, info, nil)
	mux := testMux(h)
	cancel()
	for _, r := range []*http.Request{request("POST", createPath, createBody, "key", "application/json"), request("GET", getPath, "", "", ""), request("POST", applyPath, applyBody, "key", "application/json")} {
		problem(t, serve(mux, r.WithContext(ctx)), 503, "config-plan-unavailable", "")
	}
}

func TestConfigPlanHTTPRequiredCapabilities(t *testing.T) {
	for _, run := range []func(){func() { New(nil, nil, nil) }, func() {
		New(nil, func(*http.Request) RequestInfo { return testInfo }, nil).Register(http.NewServeMux(), nil)
	}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("missing capability accepted")
				}
			}()
			run()
		}()
	}
	h := New(fakePlans{}, func(*http.Request) RequestInfo { t.Fatal("denied guard entered Handler"); return RequestInfo{} }, nil)
	mux := http.NewServeMux()
	var actions []string
	h.Register(mux, func(action string, _ http.HandlerFunc) http.HandlerFunc {
		actions = append(actions, action)
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }
	})
	if !reflect.DeepEqual(actions, []string{"config.plan", "config.review", "config.apply"}) {
		t.Fatal(actions)
	}
	for _, path := range []string{createPath, applyPath} {
		if w := serve(mux, request("POST", path, "{", "", "")); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := serve(mux, request("GET", getPath, "", "", "")); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
