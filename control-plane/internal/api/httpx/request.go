package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

func PageSize(r *http.Request, fallback int) (int, bool) {
	value := r.URL.Query().Get("page_size")
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed >= 1 && parsed <= 200
}

func ParseOptionalUUIDv7(value string) (uuid.UUID, bool) {
	if value == "" {
		return uuid.Nil, true
	}
	id, err := uuid.Parse(value)
	return id, err == nil && id.Version() == 7
}

// DecodeStrictJSON is the single request-body decoder for JSON endpoints.
// The media type must be application/json (parameters such as charset are
// allowed) so a form or text payload cannot ride through a JSON parser,
// unknown fields and anything after the first JSON value are rejected, and
// the error response is already written when it returns false.
func DecodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		WriteProblem(w, r, http.StatusUnsupportedMediaType, "https://ocservia.dev/problems/unsupported-media-type", "Unsupported media type", "Content-Type must be application/json")
		return false
	}
	// limitBody has already bounded the request. Check before encoding/json
	// can silently replace malformed UTF-8 in strings with U+FFFD.
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, http.StatusRequestEntityTooLarge, "https://ocservia.dev/problems/request-too-large", "Request too large", "request body exceeds the size limit")
		return false
	}
	if err != nil || !utf8.Valid(body) {
		WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must be valid UTF-8 JSON")
		return false
	}
	if hasDuplicateKey(body) {
		WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must not repeat an object member")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Invalid request", "request body must contain one JSON value")
		return false
	}
	return true
}

// hasDuplicateKey reports whether any object in the bounded body repeats a
// member name, which encoding/json would otherwise resolve as last-wins.
// Malformed JSON is left to the decoder's own error. Only exact repeats count:
// case-only variants are distinct map keys, while for struct fields
// DisallowUnknownFields and the field match still decide.
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

func ParseUUIDv7(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id.Version() != 7 {
		return uuid.Nil, errors.New("not UUIDv7")
	}
	return id, nil
}

func RequireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		WriteProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return "", false
	}
	return key, true
}
