package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReadinessRequiresDatabaseConnectivityIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if url == "" || ownerURL == "" {
		t.Skip("isolated runtime and owner database URLs are required")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := NewBackend("127.0.0.1:0", postgres.WrapPool(pool), BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
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
	owner, err := pgxpool.New(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
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
