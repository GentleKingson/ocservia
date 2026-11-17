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
