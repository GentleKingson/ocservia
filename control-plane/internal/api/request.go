package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return httpx.DecodeStrictJSON(w, r, target)
}

func parseUUIDv7(value string) (uuid.UUID, error) {
	return httpx.ParseUUIDv7(value)
}
func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	return httpx.RequireIdempotencyKey(w, r)
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(data[:])
}

func pageSize(r *http.Request, fallback int) (int, bool) {
	return httpx.PageSize(r, fallback)
}

func requestTraceparent(r *http.Request) string {
	span := trace.SpanContextFromContext(r.Context())
	if span.IsValid() {
		flags := "00"
		if span.IsSampled() {
			flags = "01"
		}
		return fmt.Sprintf("00-%s-%s-%s", span.TraceID(), span.SpanID(), flags)
	}
	traceID := correlationHex(32)
	spanID := correlationHex(16)
	return "00-" + traceID + "-" + spanID + "-01"
}

func correlationHex(length int) string {
	id, err := uuid.NewV7()
	if err != nil {
		return strings.Repeat("1", length)
	}
	return strings.ReplaceAll(id.String(), "-", "")[:length]
}

func parseEventID(value string) (uuid.UUID, bool) {
	return httpx.ParseOptionalUUIDv7(value)
}
