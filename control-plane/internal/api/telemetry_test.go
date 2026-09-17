package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTelemetryHistoryErrors(t *testing.T) {
	const internalError = "database connection failed: private-db.internal secret-diagnostic"
	config, err := pgxpool.ParseConfig("postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	config.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
		return errors.New(internalError)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, tc := range []struct {
		name, query, detail string
		status              int
	}{
		{"metric", "metric=invalid", "metric is invalid", http.StatusBadRequest},
		{"resolution", "metric=cpu_usage_ratio&resolution=invalid", "resolution is invalid", http.StatusBadRequest},
		{"since", "metric=cpu_usage_ratio&since=invalid", "since must be an RFC 3339 timestamp", http.StatusBadRequest},
		{"positive infinity", "metric=cpu_usage_ratio&since=infinity", "telemetry history could not be read", http.StatusServiceUnavailable},
		{"negative infinity", "metric=cpu_usage_ratio&since=-infinity", "telemetry history could not be read", http.StatusServiceUnavailable},
		{"extended year", "metric=cpu_usage_ratio&since=%2B010000-01-01T00:00:00Z", "telemetry history could not be read", http.StatusServiceUnavailable},
		{"internal", "metric=cpu_usage_ratio", "telemetry history could not be read", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			config := testHTTPConfig(true)
			config.BodyLimit, config.RequestTimeout = 1024, time.Second
			server, err := NewServer(config, nil, BuildInfo{}, slog.New(slog.NewTextHandler(&logs, nil)), Modules{Nodes: telemetry.NewBackend(postgres.WrapPool(pool))}, Authorization{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+uuid.Must(uuid.NewV7()).String()+"/telemetry?"+tc.query, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(response, request)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.detail) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), internalError) {
				t.Fatal("internal error exposed in response")
			}
			if tc.name == "internal" && !strings.Contains(logs.String(), internalError) {
				t.Fatalf("internal diagnostic missing from server log: %s", logs.String())
			}
		})
	}
}
