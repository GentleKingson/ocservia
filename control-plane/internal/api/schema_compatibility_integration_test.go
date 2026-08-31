package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReadinessHonorsSchemaCompatibilityContractIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	expected, err := migrations.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	server := New("127.0.0.1:0", pool, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", expected)
	defer server.closeEventStreams()

	reset := func() {
		if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version > $1", expected); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE controller_schema_compatibility SET \"current_schema\" = $1, minimum_compatible_controller_schema = $1 WHERE singleton", expected); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	defer reset()

	readyStatus := func(label string, instance *Server, want int) map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		instance.ready(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != want {
			t.Fatalf("%s status = %d, want %d: %s", label, response.Code, want, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s response: %v", label, err)
		}
		return body
	}

	body := readyStatus("exact baseline", server, http.StatusOK)
	if body["schema_version"] != float64(expected) {
		t.Fatalf("exact baseline schema_version = %v, want %d", body["schema_version"], expected)
	}

	future := expected + 1
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations(version, name, checksum) VALUES($1, $2, $3)", future, "000030_future.up.sql", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE controller_schema_compatibility SET \"current_schema\" = $1, minimum_compatible_controller_schema = $2 WHERE singleton", future, expected); err != nil {
		t.Fatal(err)
	}
	body = readyStatus("declared compatible newer schema", server, http.StatusOK)
	if body["schema_version"] != float64(future) {
		t.Fatalf("compatible newer schema_version = %v, want %d", body["schema_version"], future)
	}
	oldServer := New("127.0.0.1:0", pool, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", expected-1)
	defer oldServer.closeEventStreams()
	readyStatus("Controller below declared minimum", oldServer, http.StatusServiceUnavailable)

	if _, err := pool.Exec(ctx, "UPDATE controller_schema_compatibility SET \"current_schema\" = $1, minimum_compatible_controller_schema = $1 WHERE singleton", expected); err != nil {
		t.Fatal(err)
	}
	readyStatus("newer schema without declaration", server, http.StatusServiceUnavailable)

	reset()
	newServer := New("127.0.0.1:0", pool, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "", future)
	defer newServer.closeEventStreams()
	readyStatus("database older than Controller", newServer, http.StatusServiceUnavailable)
}
