package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/jackc/pgx/v5"
)

func TestReadinessRequiresDatabaseConnectivityIntegration(t *testing.T) {
	pool := testfixture.OpenRuntimePool(t)
	owner := testfixture.OpenOwnerPool(t)
	server, err := NewServer(HTTPConfig{Address: "127.0.0.1:0", BodyLimit: 1024, RequestTimeout: time.Second, DevAuth: false, DevAuthToken: "", EventStreams: eventstream.DefaultConfig()}, postgres.WrapPool(pool), BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Modules{}, Authorization{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.closeEventStreams()
	check := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		server.ready(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != want {
			t.Fatalf("readiness status=%d want=%d: %s", response.Code, want, response.Body.String())
		}
	}
	check(http.StatusOK)
	var role string
	if err := pool.QueryRow(context.Background(), "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	grant := "GRANT SELECT ON operations TO " + pgx.Identifier{role}.Sanitize()
	defer func() {
		if _, err := owner.Exec(context.Background(), grant); err != nil {
			t.Error(err)
		}
	}()
	if _, err := owner.Exec(context.Background(), "REVOKE SELECT ON operations FROM "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	check(http.StatusServiceUnavailable)
	if _, err := owner.Exec(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	check(http.StatusOK)
	pool.Close()
	check(http.StatusServiceUnavailable)
}
