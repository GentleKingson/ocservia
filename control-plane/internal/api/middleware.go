package api

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

func (s *Server) timeout(next http.Handler) http.Handler {
	next = s.trackRequests(next)
	timed := http.TimeoutHandler(next, s.requestTimeout, `{"type":"https://ocservia.dev/problems/timeout","title":"Request timed out","status":503}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events/stream" || strings.HasSuffix(r.URL.Path, "/events") && strings.HasPrefix(r.URL.Path, "/api/v1/operations/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		timed.ServeHTTP(w, r)
	})
}

func (s *Server) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestsMu.Lock()
		if s.stopping {
			s.requestsMu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.requests.Add(1)
		s.requestsMu.Unlock()
		defer s.requests.Done()
		next.ServeHTTP(w, r)
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
		if requestID == "" || len(requestID) > 128 || boundedLogField(requestID, 128) != requestID {
			requestID = randomID()
		}
		w.Header().Set("X-Request-ID", requestID)
		if s.hasOperationPrincipal(r) {
			w.Header().Set("X-Ocservia-Dev-Subject", "developer")
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		if !authResultRoute(r) {
			s.logger.InfoContext(r.Context(), "http request", "request_id", requestID, "trace_id", trace.SpanContextFromContext(r.Context()).TraceID().String(), "method", r.Method, "path", r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
