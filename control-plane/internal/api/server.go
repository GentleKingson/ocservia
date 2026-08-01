package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Role    string `json:"role"`
}

type Server struct {
	http           *http.Server
	pool           *pgxpool.Pool
	build          BuildInfo
	logger         *slog.Logger
	bodyLimit      int64
	requestTimeout time.Duration
	devAuth        bool
}

func New(address string, pool *pgxpool.Pool, build BuildInfo, logger *slog.Logger, bodyLimit int64, requestTimeout time.Duration, devAuth bool) *Server {
	s := &Server{pool: pool, build: build, logger: logger, bodyLimit: bodyLimit, requestTimeout: requestTimeout, devAuth: devAuth}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.live)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /api/v1/livez", s.live)
	mux.HandleFunc("GET /api/v1/readyz", s.ready)
	mux.HandleFunc("GET /api/v1/version", s.version)
	handler := s.requestContext(s.limitBody(s.timeout(s.routeErrors(mux))))
	s.http = &http.Server{Addr: address, Handler: otelhttp.NewHandler(handler, "http.server"), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}

func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is not ready", "database dependency is unavailable")
		return
	}
	version, err := migrations.CurrentSchemaVersion(ctx, s.pool)
	if err != nil || version < 1 {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/schema-unavailable", "Service is not ready", "database schema is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "schema_version": version})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.build)
}

func (s *Server) routeErrors(next http.Handler) http.Handler {
	paths := map[string]struct{}{"/livez": {}, "/readyz": {}, "/version": {}, "/api/v1/livez": {}, "/api/v1/readyz": {}, "/api/v1/version": {}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := paths[r.URL.Path]; !ok {
			writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeProblem(w, r, http.StatusMethodNotAllowed, "https://ocservia.dev/problems/method-not-allowed", "Method not allowed", "the requested method is not supported")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) timeout(next http.Handler) http.Handler {
	timed := http.TimeoutHandler(next, s.requestTimeout, `{"type":"https://ocservia.dev/problems/timeout","title":"Request timed out","status":503}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		timed.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = randomID()
		}
		w.Header().Set("X-Request-ID", requestID)
		if s.devAuth {
			w.Header().Set("X-Ocservia-Dev-Subject", "developer")
		}
		s.logger.InfoContext(r.Context(), "http request", "request_id", requestID, "trace_id", trace.SpanContextFromContext(r.Context()).TraceID().String(), "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, kind, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "title": title, "status": status, "detail": detail, "instance": r.URL.Path, "trace_id": trace.SpanContextFromContext(r.Context()).TraceID().String()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(data[:])
}
