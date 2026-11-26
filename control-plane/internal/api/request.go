package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}

func pageSize(r *http.Request, fallback int) (int, bool) {
	value := r.URL.Query().Get("page_size")
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed >= 1 && parsed <= 200
}

func parseOptionalUUIDv7(value string) (uuid.UUID, bool) {
	if value == "" {
		return uuid.Nil, true
	}
	id, err := uuid.Parse(value)
	return id, err == nil && id.Version() == 7
}

func requirePathUUIDv7(w http.ResponseWriter, r *http.Request, param, suffix string) (uuid.UUID, bool) {
	raw := r.PathValue(param)
	if suffix != "" {
		raw = strings.TrimSuffix(raw, suffix)
	}
	id, err := parseUUIDv7(raw)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Invalid identifier", param+" must be a UUIDv7")
		return uuid.Nil, false
	}
	return id, true
}

func parseExpectedRevision(ifMatch string, explicit *int64, minValue int64) (int64, bool) {
	var header *int64
	if ifMatch != "" {
		if len(ifMatch) < 3 || ifMatch[0] != '"' || ifMatch[len(ifMatch)-1] != '"' {
			return 0, false
		}
		value := ifMatch[1 : len(ifMatch)-1]
		if strings.ContainsRune(value, '"') || !strings.HasPrefix(value, "revision-") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "revision-"), 10, 64)
		if err != nil || parsed < minValue {
			return 0, false
		}
		header = &parsed
	}
	if explicit != nil && *explicit < minValue {
		return 0, false
	}
	if explicit != nil && header != nil && *explicit != *header {
		return 0, false
	}
	if explicit != nil {
		return *explicit, true
	}
	if header != nil {
		return *header, true
	}
	return 0, false
}

func writeCursorPage[T any](w http.ResponseWriter, items []T, hasMore bool, cursorOf func(T) string) {
	page := map[string]any{"has_more": hasMore}
	if hasMore && len(items) > 0 {
		page["next_cursor"] = cursorOf(items[len(items)-1])
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page})
}

func writeAcceptedOperation(w http.ResponseWriter, op operations.Operation, replayed, withETag bool) {
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID)
	if withETag {
		w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", op.Version))
	}
	writeJSON(w, http.StatusAccepted, op)
}

func writeCommonDomainError(w http.ResponseWriter, r *http.Request, err error, notFoundDetail string) bool {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/forbidden", "Access denied", "the session or resource authority is no longer valid")
		return true
	case errors.Is(err, database.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", notFoundDetail)
		return true
	default:
		return false
	}
}

// decodeStrictJSON is the single request-body decoder for JSON endpoints.
// The media type must be application/json (parameters such as charset are
// allowed) so a form or text payload cannot ride through a JSON parser,
// unknown fields and anything after the first JSON value are rejected, and
// the error response is already written when it returns false.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "https://ocservia.dev/problems/unsupported-media-type", "Unsupported media type", "Content-Type must be application/json")
		return false
	}
	// limitBody has already bounded the request. Check before encoding/json
	// can silently replace malformed UTF-8 in strings with U+FFFD.
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "https://ocservia.dev/problems/request-too-large", "Request too large", "request body exceeds the size limit")
		return false
	}
	if err != nil || !utf8.Valid(body) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must be valid UTF-8 JSON")
		return false
	}
	if hasDuplicateKey(body) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must not repeat an object member")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must contain one JSON value")
		return false
	}
	return true
}

// hasDuplicateKey reports whether any object in the bounded body repeats a
// member name, which encoding/json would otherwise resolve as last-wins.
func hasDuplicateKey(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool
	}
	var stack []*frame
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				if top != nil && top.object {
					top.key = true
				}
				stack = append(stack, &frame{object: value == '{', keys: map[string]bool{}, key: true})
			default:
				stack = stack[:len(stack)-1]
			}
			continue
		case string:
			if top != nil && top.object && top.key {
				if top.keys[value] {
					return true
				}
				top.keys[value] = true
				top.key = false
				continue
			}
		}
		if top != nil && top.object {
			top.key = true
		}
	}
}

func parseUUIDv7(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id.Version() != 7 {
		return uuid.Nil, errors.New("not UUIDv7")
	}
	return id, nil
}

func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return "", false
	}
	return key, true
}

func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(data[:])
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
