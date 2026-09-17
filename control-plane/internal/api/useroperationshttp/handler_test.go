package useroperationshttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

type operationsStub struct {
	getPolicy   func(context.Context, uuid.UUID, string) (useroperations.Policy, error)
	setPolicy   func(context.Context, useroperations.PolicyRequest) (useroperations.Policy, bool, error)
	createBatch func(context.Context, useroperations.BatchRequest) (useroperations.Batch, bool, error)
	getBatch    func(context.Context, uuid.UUID) (useroperations.Batch, error)
	metrics     func(context.Context, uuid.UUID) (useroperations.Metrics, error)
}

func (s operationsStub) GetPolicy(c context.Context, id uuid.UUID, name string) (useroperations.Policy, error) {
	return s.getPolicy(c, id, name)
}
func (s operationsStub) SetPolicy(c context.Context, r useroperations.PolicyRequest) (useroperations.Policy, bool, error) {
	return s.setPolicy(c, r)
}
func (s operationsStub) CreateBatch(c context.Context, r useroperations.BatchRequest) (useroperations.Batch, bool, error) {
	return s.createBatch(c, r)
}
func (s operationsStub) GetBatch(c context.Context, id uuid.UUID) (useroperations.Batch, error) {
	return s.getBatch(c, id)
}
func (s operationsStub) Metrics(c context.Context, id uuid.UUID) (useroperations.Metrics, error) {
	return s.metrics(c, id)
}

type authorizerStub struct {
	node      func(context.Context, uuid.UUID) (rbac.Resource, error)
	authorize func(context.Context, uuid.UUID, string, rbac.Resource, bool) error
}

func (a authorizerStub) Node(c context.Context, id uuid.UUID) (rbac.Resource, error) {
	return a.node(c, id)
}
func (a authorizerStub) Authorize(c context.Context, id uuid.UUID, action string, r rbac.Resource, glass bool) error {
	return a.authorize(c, id, action, r, glass)
}

var nodeID = uuid.MustParse("019fc0a4-6d92-765c-a8a1-4af556614cc3")

const policyPath = "/api/v1/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3/users/alice/policy"
const batchPath = "/api/v1/user-batches/019fc0a4-6d92-765c-a8a1-4af556614cc3"

func fixtureInfo() RequestInfo {
	return RequestInfo{Principal: auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), Issuer: auth.LocalIssuer, BreakGlass: true}, WorkspaceID: uuid.Must(uuid.NewV7()), ActorID: "operator", RequestID: "request", Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", ApprovalID: uuid.Must(uuid.NewV7())}
}

func module(info *RequestInfo) (*Handler, *http.ServeMux) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), func(*http.Request) RequestInfo { return *info })
	mux := http.NewServeMux()
	h.Register(mux, func(_ string, next http.HandlerFunc) http.HandlerFunc { return next })
	return h, mux
}

func call(mux *http.ServeMux, ctx context.Context, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func problem(t *testing.T, w *httptest.ResponseRecorder, status int, kind, detail string) {
	t.Helper()
	var p struct {
		Type, Detail string
		Status       int
	}
	if json.Unmarshal(w.Body.Bytes(), &p) != nil || w.Code != status || p.Status != status || p.Type != "https://ocservia.dev/problems/"+kind || (detail != "" && p.Detail != detail) || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("want %d %s %s: %d %v %s", status, kind, detail, w.Code, w.Header(), w.Body)
	}
	for _, header := range []string{"ETag", "Location", "Idempotency-Replayed"} {
		if w.Header().Get(header) != "" {
			t.Fatal("failure has success header", header)
		}
	}
}

func TestUserOperationsHTTPContracts(t *testing.T) {
	info := fixtureInfo()
	h, mux := module(&info)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checkContext := func(got context.Context) {
		t.Helper()
		if got != ctx {
			t.Fatal("original context lost")
		}
	}
	policy := useroperations.Policy{NodeID: nodeID, Username: "alice", Version: 9, QuotaBytes: 123}
	batch := useroperations.Batch{ID: nodeID, WorkspaceID: info.WorkspaceID, ActorIdentityID: &info.Principal.IdentityID}
	expires, _ := time.Parse(time.RFC3339, "2030-01-02T03:04:05.123456Z")
	var calls []string
	h.SetOperations(operationsStub{
		getPolicy: func(c context.Context, id uuid.UUID, name string) (useroperations.Policy, error) {
			checkContext(c)
			calls = append(calls, "getPolicy")
			if id != nodeID || name != "alice" {
				t.Fatal(id, name)
			}
			return policy, nil
		},
		setPolicy: func(c context.Context, r useroperations.PolicyRequest) (useroperations.Policy, bool, error) {
			checkContext(c)
			calls = append(calls, "setPolicy")
			want := useroperations.PolicyRequest{NodeID: nodeID, Username: "alice", QuotaPeriod: "monthly", QuotaDirection: "rxtx", QuotaBytes: 123, ExpectedVersion: 8, ExpiresAt: &expires, IdempotencyKey: "key", Reason: "reason", ActorID: info.ActorID, ActorIdentityID: info.Principal.IdentityID, ActorSessionID: info.Principal.SessionID, RequestID: info.RequestID, Traceparent: info.Traceparent}
			if !reflect.DeepEqual(r, want) {
				t.Fatalf("policy request: %+v want %+v", r, want)
			}
			return policy, true, nil
		},
		createBatch: func(c context.Context, r useroperations.BatchRequest) (useroperations.Batch, bool, error) {
			checkContext(c)
			calls = append(calls, "createBatch")
			want := useroperations.BatchRequest{WorkspaceID: info.WorkspaceID, ActorIdentityID: info.Principal.IdentityID, ActorSessionID: info.Principal.SessionID, ApprovalID: info.ApprovalID, ActorID: info.ActorID, Reason: "reason", RequestID: info.RequestID, Traceparent: info.Traceparent, IdempotencyKey: "key", Items: []useroperations.BatchItemRequest{{NodeID: nodeID, Username: "alice", Action: "disable", ExpectedVersion: 3, Authorized: true}}}
			if !reflect.DeepEqual(r, want) {
				t.Fatalf("batch request: %+v want %+v", r, want)
			}
			return batch, true, nil
		},
		getBatch: func(c context.Context, id uuid.UUID) (useroperations.Batch, error) {
			checkContext(c)
			calls = append(calls, "getBatch")
			if id != nodeID {
				t.Fatal(id)
			}
			return batch, nil
		},
		metrics: func(c context.Context, id uuid.UUID) (useroperations.Metrics, error) {
			checkContext(c)
			calls = append(calls, "metrics")
			if id != info.WorkspaceID {
				t.Fatal(id)
			}
			return useroperations.Metrics{}, nil
		},
	})
	h.SetAuthorizer(authorizerStub{
		node: func(c context.Context, id uuid.UUID) (rbac.Resource, error) {
			checkContext(c)
			if id != nodeID {
				t.Fatal(id)
			}
			return rbac.Resource{WorkspaceID: info.WorkspaceID, Type: "node", ID: id}, nil
		},
		authorize: func(c context.Context, id uuid.UUID, action string, r rbac.Resource, glass bool) error {
			checkContext(c)
			if id != info.Principal.IdentityID || action != "user.manage" || !glass || r.ID != nodeID {
				t.Fatal("authorization inputs", id, action, r, glass)
			}
			return nil
		},
	})
	for _, tc := range []struct {
		method, path, body     string
		status                 int
		etag, location, replay string
	}{
		{"GET", policyPath, "", 200, "", "", ""},
		{"PUT", policyPath, `{"quota_period":"monthly","quota_direction":"rxtx","quota_bytes":123,"expected_version":8,"expires_at":"2030-01-02T03:04:05.123456Z","reason":" reason "}`, 200, `"revision-9"`, "", "true"},
		{"POST", "/api/v1/user-batches", fmt.Sprintf(`{"reason":" reason ","items":[{"node_id":%q,"username":"alice","action":"disable","expected_version":3}]}`, nodeID), 202, "", batchPath, "true"},
		{"GET", batchPath, "", 200, "", "", ""},
		{"GET", "/api/v1/user-operations/metrics", "", 200, "", "", ""},
	} {
		w := call(mux, ctx, tc.method, tc.path, tc.body, " key ")
		if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("ETag") != tc.etag || w.Header().Get("Location") != tc.location || w.Header().Get("Idempotency-Replayed") != tc.replay {
			t.Fatalf("response: %d %v %s", w.Code, w.Header(), w.Body)
		}
	}
	if !reflect.DeepEqual(calls, []string{"getPolicy", "setPolicy", "createBatch", "getBatch", "metrics"}) {
		t.Fatal(calls)
	}
}

func TestUserOperationsHTTPValidation(t *testing.T) {
	for _, tc := range []struct {
		name, expiry string
		accepted     bool
	}{
		{"absent", "", true}, {"null", `,"expires_at":null`, true},
		{"utc", `,"expires_at":"2030-01-02T03:04:05Z"`, true},
		{"fraction", `,"expires_at":"2030-01-02T03:04:05.123456789Z"`, true},
		{"empty", `,"expires_at":""`, false}, {"offset", `,"expires_at":"2030-01-02T03:04:05+00:00"`, false},
		{"extended", `,"expires_at":"12030-01-02T03:04:05Z"`, false}, {"infinity", `,"expires_at":"infinity"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := fixtureInfo()
			h, mux := module(&info)
			called := false
			h.SetOperations(operationsStub{setPolicy: func(_ context.Context, r useroperations.PolicyRequest) (useroperations.Policy, bool, error) {
				called = true
				if (r.ExpiresAt == nil) != (tc.name == "absent" || tc.name == "null") {
					t.Fatal("expiry presence", r.ExpiresAt)
				}
				return useroperations.Policy{Version: 1}, false, nil
			}})
			w := call(mux, t.Context(), "PUT", policyPath, `{"reason":"test"`+tc.expiry+`}`, "key")
			if called != tc.accepted {
				t.Fatal("service entry", called)
			}
			if tc.accepted {
				if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "" {
					t.Fatal(w.Code, w.Body)
				}
			} else {
				problem(t, w, 400, "invalid-request", "expires_at must be an RFC 3339 UTC timestamp ending in Z")
			}
		})
	}
	info := fixtureInfo()
	h, mux := module(&info)
	h.SetOperations(operationsStub{}) // Any unexpected domain entry panics.
	h.SetAuthorizer(authorizerStub{})
	for _, tc := range []struct {
		method, path, body, key, media string
		status                         int
		kind, detail                   string
	}{
		{"PUT", "/api/v1/nodes/bad/users/alice/policy", "{", "", "", 400, "invalid-id", "node_id must be a UUIDv7"},
		{"GET", "/api/v1/nodes/bad/users/alice/policy", "", "", "", 400, "invalid-id", "node_id must be a UUIDv7"},
		{"GET", "/api/v1/user-batches/bad", "", "", "", 400, "invalid-id", "batch_id must be a UUIDv7"},
		{"PUT", policyPath, "{", "", "", 400, "idempotency-key-required", "Idempotency-Key must be provided"},
		{"POST", "/api/v1/user-batches", "{", "", "", 400, "idempotency-key-required", "Idempotency-Key must be provided"},
		{"PUT", policyPath, "{", "key", "text/plain", 415, "unsupported-media-type", "Content-Type must be application/json"},
		{"PUT", policyPath, `{"quota_bytes":1.5}`, "key", "application/json", 400, "invalid-request", "request body is invalid"},
		{"PUT", policyPath, `{"quota_bytes":9223372036854775808}`, "key", "application/json", 400, "invalid-request", "request body is invalid"},
		{"PUT", policyPath, `{} {}`, "key", "application/json", 400, "invalid-request", "request body must contain one JSON value"},
		{"PUT", policyPath, "{\"reason\":\"\xff\"}", "key", "application/json", 400, "invalid-request", "request body must be valid UTF-8 JSON"},
		{"POST", "/api/v1/user-batches", `{"items":[{"authorized":true}]}`, "key", "application/json", 400, "invalid-request", "request body is invalid"},
		{"POST", "/api/v1/user-batches", `{"approval_id":"anything"}`, "key", "application/json", 400, "invalid-request", "request body is invalid"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Idempotency-Key", tc.key)
		r.Header.Set("Content-Type", tc.media)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		problem(t, w, tc.status, tc.kind, tc.detail)
	}
}

func TestUserOperationsHTTPAuthorization(t *testing.T) {
	for _, mode := range []string{"per-item", "development", "missing-node", "foreign-node"} {
		t.Run(mode, func(t *testing.T) {
			info := fixtureInfo()
			h, mux := module(&info)
			second := uuid.Must(uuid.NewV7())
			called, checks := false, 0
			if mode == "development" {
				info.Principal.Issuer = "development"
			}
			h.SetAuthorizer(authorizerStub{
				node: func(c context.Context, id uuid.UUID) (rbac.Resource, error) {
					if c != t.Context() {
						t.Fatal("node context lost")
					}
					r := rbac.Resource{WorkspaceID: info.WorkspaceID, Type: "node", ID: id}
					if id == second {
						if mode == "missing-node" {
							return r, database.ErrNotFound
						}
						if mode == "foreign-node" {
							r.WorkspaceID = uuid.Must(uuid.NewV7())
						}
					}
					return r, nil
				},
				authorize: func(c context.Context, _ uuid.UUID, _ string, r rbac.Resource, _ bool) error {
					if c != t.Context() {
						t.Fatal("authorize context lost")
					}
					checks++
					if r.ID == second {
						return rbac.ErrForbidden
					}
					return nil
				},
			})
			h.SetOperations(operationsStub{createBatch: func(_ context.Context, r useroperations.BatchRequest) (useroperations.Batch, bool, error) {
				called = true
				if len(r.Items) != 3 || r.Items[0].NodeID != nodeID || !r.Items[0].Authorized || r.Items[1].NodeID != second || r.Items[1].Authorized != (mode == "development") || r.Items[2].NodeID != nodeID || r.Items[2].ExpectedVersion != 8 {
					t.Fatal("item order/authorization/version changed", r.Items)
				}
				return useroperations.Batch{ID: nodeID}, false, nil
			}})
			body := fmt.Sprintf(`{"items":[{"node_id":%q},{"node_id":%q},{"node_id":%q,"expected_version":8}]}`, nodeID, second, nodeID)
			w := call(mux, t.Context(), "POST", "/api/v1/user-batches", body, "key")
			if mode == "missing-node" || mode == "foreign-node" {
				problem(t, w, 400, "invalid-request", "every batch item must reference an existing user in the selected workspace")
				if called {
					t.Fatal("rejected batch entered service")
				}
			} else if !called || w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			if mode == "development" && checks != 0 {
				t.Fatal("development issuer should skip per-item Authorize")
			}
		})
	}
	for _, mode := range []string{"creator", "development", "reader", "denied", "lookup-error", "foreign", "missing-authorizer", "authorization-not-found"} {
		t.Run(mode, func(t *testing.T) {
			info := fixtureInfo()
			h, mux := module(&info)
			checks := 0
			batch := useroperations.Batch{ID: nodeID, WorkspaceID: info.WorkspaceID}
			if mode == "creator" {
				batch.ActorIdentityID = &info.Principal.IdentityID
			}
			if mode == "development" {
				info.Principal.Issuer = "development"
			}
			if mode == "foreign" {
				batch.WorkspaceID = uuid.Nil
			}
			h.SetOperations(operationsStub{getBatch: func(context.Context, uuid.UUID) (useroperations.Batch, error) {
				if mode == "lookup-error" {
					return batch, errors.New("storage")
				}
				return batch, nil
			}})
			if mode != "missing-authorizer" {
				h.SetAuthorizer(authorizerStub{authorize: func(c context.Context, id uuid.UUID, action string, r rbac.Resource, glass bool) error {
					checks++
					if c != t.Context() || id != info.Principal.IdentityID || action != "operation.read" || r != (rbac.Resource{WorkspaceID: info.WorkspaceID, Type: "workspace"}) || !glass {
						t.Fatal("workspace check inputs")
					}
					if mode == "denied" {
						return rbac.ErrForbidden
					}
					if mode == "authorization-not-found" {
						return database.ErrNotFound
					}
					return nil
				}})
			}
			w := call(mux, t.Context(), "GET", batchPath, "", "")
			switch mode {
			case "lookup-error", "foreign":
				problem(t, w, 404, "not-found", "the requested batch does not exist")
			case "denied":
				problem(t, w, 403, "forbidden", "the principal is not authorized for this resource and action")
			case "authorization-not-found":
				problem(t, w, 404, "not-found", "the requested resource does not exist")
			case "missing-authorizer":
				problem(t, w, 503, "service-unavailable", "user operations service is unavailable")
			default:
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			}
			wantChecks := 0
			if mode == "reader" || mode == "denied" || mode == "authorization-not-found" {
				wantChecks = 1
			}
			if checks != wantChecks {
				t.Fatal("check order", checks)
			}
		})
	}
}

func TestUserOperationsHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		err          error
		status       int
		kind, detail string
	}{
		{useroperations.ErrInvalidRequest, 400, "invalid-request", "the quota, expiry, or batch request failed validation"},
		{useroperations.ErrVersionConflict, 409, "stale-revision", "the user policy changed after this request was prepared"},
		{useroperations.ErrIdempotencyConflict, 409, "idempotency-conflict", "the Idempotency-Key was already used with different input"},
		{useroperations.ErrNotFound, 404, "not-found", "the requested user policy or batch does not exist"},
		{database.ErrNotFound, 404, "not-found", "the requested user policy or batch does not exist"},
		{rbac.ErrForbidden, 403, "forbidden", "the principal is not authorized for this operation"},
		{approvals.ErrNotReady, 403, "approval-required", "bulk user disable requires a valid approval bound to the batch identifier"},
		{errors.New("storage"), 503, "database-unavailable", "user operations state is temporarily unavailable"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			info := fixtureInfo()
			h, mux := module(&info)
			h.SetOperations(operationsStub{getPolicy: func(context.Context, uuid.UUID, string) (useroperations.Policy, error) {
				return useroperations.Policy{}, fmt.Errorf("wrapped: %w", tc.err)
			}})
			problem(t, call(mux, t.Context(), "GET", policyPath, "", ""), tc.status, tc.kind, tc.detail)
		})
	}
}

func TestUserOperationsHTTPRequiredCapabilities(t *testing.T) {
	info := fixtureInfo()
	h, mux := module(&info)
	for _, route := range []struct{ method, path string }{{"GET", policyPath}, {"PUT", policyPath}, {"POST", "/api/v1/user-batches"}, {"GET", batchPath}, {"GET", "/api/v1/user-operations/metrics"}} {
		problem(t, call(mux, t.Context(), route.method, route.path, "{", ""), 503, "service-unavailable", "user operations service is unavailable")
	}
	h.SetOperations(operationsStub{})
	problem(t, call(mux, t.Context(), "POST", "/api/v1/user-batches", "{", ""), 503, "service-unavailable", "user operations service is unavailable")
	for _, missing := range []string{"guard", "request-info"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("missing required capability accepted")
				}
			}()
			if missing == "guard" {
				h.Register(http.NewServeMux(), nil)
			} else {
				New(slog.Default(), nil)
			}
		})
	}
	var actions []string
	mux = http.NewServeMux()
	h.Register(mux, func(action string, _ http.HandlerFunc) http.HandlerFunc {
		actions = append(actions, action)
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }
	})
	if !reflect.DeepEqual(actions, []string{"node.read", "user.manage", "user.manage", "operation.read", "operation.read"}) {
		t.Fatal(actions)
	}
	if w := call(mux, t.Context(), "GET", policyPath, "", ""); w.Code != 401 {
		t.Fatal("guard bypassed")
	}
}
