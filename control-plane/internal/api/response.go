package api

import (
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

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
