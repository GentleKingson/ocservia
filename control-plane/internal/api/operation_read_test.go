package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
	"github.com/google/uuid"
)

type operationReadBackend struct {
	database.Backend
	store *operationReadStore
}

func (b operationReadBackend) Begin(context.Context, database.Isolation) (database.Tx, error) {
	return operationReadTx{store: b.store}, nil
}

type operationReadTx struct {
	database.Tx
	store *operationReadStore
}

func (tx operationReadTx) Commit(context.Context) error         { return nil }
func (tx operationReadTx) Rollback(context.Context) error       { return nil }
func (tx operationReadTx) OperationStore() operationstore.Store { return tx.store }

type operationReadStore struct {
	operationstore.Store
	operation operations.Operation
	items     []operations.Operation
	err       error
	calls     int
	workspace uuid.UUID
	after     uuid.UUID
	limit     int
}

func (s *operationReadStore) Get(_ context.Context, id uuid.UUID) (operations.Operation, error) {
	s.calls++
	s.after = id
	return s.operation, s.err
}

func (s *operationReadStore) ListInWorkspace(_ context.Context, workspace, after uuid.UUID, limit int) ([]operations.Operation, error) {
	s.calls++
	s.workspace, s.after, s.limit = workspace, after, limit
	return s.items, s.err
}

func (s *operationReadStore) SummaryInWorkspace(_ context.Context, workspace uuid.UUID) (operationstore.Summary, error) {
	s.calls++
	s.workspace = workspace
	return operationstore.Summary{Active: 6, Unknown: 2}, s.err
}

func TestOperationReadsUseOperationsWithoutTransportEvents(t *testing.T) {
	node, command := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	stamp := value.Timestamp{Valid: true, Micros: 1}
	op := operations.Operation{
		ID: uuid.Must(uuid.NewV7()).String(), State: "unknown", NodeID: &node, CommandID: &command,
		Version: 7, CreatedAt: stamp, UpdatedAt: stamp, ExpiresAt: &stamp,
		ConfigApplyState: "failed", ConfigApplyFailureCode: "rollback_failed",
		AgentUpgradeState: "scheduled", AgentUpgradeTarget: "2.0.0",
	}
	store := &operationReadStore{operation: op, items: []operations.Operation{op, {ID: uuid.Must(uuid.NewV7()).String()}}}
	backend := operationReadBackend{store: store}
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{}, Authorization{})
	s.EnableOperations(operations.NewBackend(backend, 50, nil))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "localhost"
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	w := get("/api/v1/operations/" + op.ID)
	var detail operations.Operation
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || !reflect.DeepEqual(detail, op) || w.Header().Get("ETag") != `"revision-7"` || store.calls != 1 || store.after.String() != op.ID {
		t.Fatalf("detail or ETag: %s headers=%v calls=%d err=%v", w.Body, w.Header(), store.calls, err)
	}
	var page struct {
		Items []operations.Operation `json:"items"`
		Page  struct {
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		} `json:"page"`
	}
	w = get("/api/v1/operations?page_size=1")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], detail) || !page.Page.HasMore || page.Page.NextCursor != op.ID || store.limit != 2 || store.workspace != uuid.Nil {
		t.Fatalf("first page: %s limit=%d scope=%s err=%v", w.Body, store.limit, store.workspace, err)
	}
	store.items = nil
	w = get("/api/v1/operations?cursor=" + op.ID)
	page.Page.NextCursor = ""
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Items == nil || len(page.Items) != 0 || page.Page.HasMore || page.Page.NextCursor != "" || store.after.String() != op.ID || store.limit != 51 {
		t.Fatalf("empty cursor page: %s err=%v", w.Body, err)
	}
	w = get("/api/v1/operations/summary")
	var summary operations.OperationSummary
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil || summary.Active != 6 || summary.Unknown != 2 || store.workspace != uuid.Nil {
		t.Fatalf("aggregate summary: %s err=%v", w.Body, err)
	}
	// The handlers must pass the trusted workspace through unchanged.
	workspace := uuid.Must(uuid.NewV7())
	for _, handler := range []http.HandlerFunc{s.listOperations, s.operationSummary} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
		r = r.WithContext(context.WithValue(r.Context(), workspaceKey{}, workspace))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK || store.workspace != workspace {
			t.Fatalf("workspace read: %d scope=%s body=%s", w.Code, store.workspace, w.Body)
		}
	}
}

func TestOperationReadErrorsDoNotFallBackToTransportEvents(t *testing.T) {
	store := &operationReadStore{}
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{}, Authorization{})
	s.EnableOperations(operations.NewBackend(operationReadBackend{store: store}, 50, nil))
	// A fallback to this service would panic because it has no database.
	s.EnableTransportEvents(transportevents.NewBackend(nil, nil))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	for _, test := range []struct {
		path, problem, title, detail string
		err                          error
		status, calls                int
	}{
		{"/api/v1/operations/invalid", "invalid-id", "Identifier is invalid", "operation_id must be a UUIDv7", nil, 400, 0},
		{"/api/v1/operations?cursor=invalid", "invalid-cursor", "Cursor is invalid", "cursor must be a UUIDv7 operation ID", nil, 400, 0},
		{"/api/v1/operations?page_size=201", "invalid-page-size", "Page size is invalid", "page_size must be an integer between 1 and 200", nil, 400, 0},
		{"/api/v1/operations/" + baselineID, "not-found", "Resource not found", "the requested operation does not exist", database.ErrNotFound, 404, 1},
		{"/api/v1/operations/" + baselineID, "database-unavailable", "Service is unavailable", "operation state is temporarily unavailable", database.ErrUnavailable, 503, 1},
		{"/api/v1/operations", "database-unavailable", "Service is unavailable", "operations are temporarily unavailable", database.ErrUnavailable, 503, 1},
		{"/api/v1/operations/summary", "database-unavailable", "Service is unavailable", "operations are temporarily unavailable", database.ErrUnavailable, 503, 1},
	} {
		store.err, store.calls = test.err, 0
		r := baselineRequest(http.MethodGet, test.path, nil)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		assertBaselineProblem(t, w, r.URL.Path, test.status, test.problem, test.title, test.detail)
		if store.calls != test.calls {
			t.Fatalf("GET %s: %d reads, want %d", test.path, store.calls, test.calls)
		}
	}
	s.EnableOperations(nil)
	for _, path := range []string{"/api/v1/operations/" + baselineID, "/api/v1/operations", "/api/v1/operations/summary"} {
		r := baselineRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		assertBaselineProblem(t, w, path, 404, "not-found", "Resource not found", "the requested resource does not exist")
	}
}
