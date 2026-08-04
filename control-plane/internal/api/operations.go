package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type syntheticCommandRequest struct {
	Kind             operationstore.SyntheticKind `json:"kind"`
	Message          string                       `json:"message,omitempty"`
	ExpectedVersion  *int64                       `json:"expected_version,omitempty"`
	TTLSeconds       *int64                       `json:"ttl_seconds,omitempty"`
	SupersedePending bool                         `json:"supersede_pending,omitempty"`
}

func (s *Server) createSyntheticCommand(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	nodeID, err := uuid.Parse(r.PathValue("node_id"))
	if err != nil || nodeID.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "node_id must be a UUIDv7")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/idempotency-key-required", "Idempotency key is required", "Idempotency-Key must be provided")
		return
	}
	var body *syntheticCommandRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body == nil {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the synthetic command request is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the request must contain one JSON object")
		return
	}
	expectedVersion, ok := expectedRevision(r.Header.Get("If-Match"), body.ExpectedVersion)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/expected-version-required", "Expected version is invalid", "provide If-Match revision-N or expected_version")
		return
	}
	ttl := int64(60)
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	if ttl < 1 || ttl > 86400 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "ttl_seconds must be between 1 and 86400")
		return
	}
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	operation, replayed, err := s.operations.CreateSynthetic(r.Context(), operationstore.CreateRequest{
		NodeID: nodeID, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion,
		Kind: body.Kind, Message: body.Message, SupersedePending: body.SupersedePending,
		TTL: time.Duration(ttl) * time.Second, RequestID: requestID, Traceparent: requestTraceparent(r),
	})
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	w.Header().Set("ETag", fmt.Sprintf("\"revision-%d\"", operation.Version))
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) streamOperationEvents(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	operationID, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil || operationID.Version() != 7 {
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-id", "Identifier is invalid", "operation_id must be a UUIDv7")
		return
	}
	after := uuid.Nil
	if cursor := strings.TrimSpace(r.Header.Get("Last-Event-ID")); cursor != "" {
		after, err = uuid.Parse(cursor)
		if err != nil || after.Version() != 7 {
			writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-cursor", "Cursor is invalid", "Last-Event-ID must be a UUIDv7")
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "https://ocservia.dev/problems/stream-unavailable", "Stream is unavailable", "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	poll := time.NewTicker(250 * time.Millisecond)
	keepalive := time.NewTicker(10 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
	controller := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			events, err := s.operations.ListEvents(r.Context(), operationID, after, 100)
			if err != nil {
				s.logger.WarnContext(r.Context(), "poll operation events", "error", err)
				continue
			}
			for _, event := range events {
				data, err := json.Marshal(event)
				if err != nil {
					return
				}
				_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(w, "id: %s\nevent: operation\ndata: %s\n\n", event.ID, data); err != nil {
					return
				}
				flusher.Flush()
				after, _ = uuid.Parse(event.ID)
			}
		}
	}
}

func (s *Server) queueMetrics(w http.ResponseWriter, r *http.Request) {
	if s.operations == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/service-unavailable", "Service is unavailable", "operation service is unavailable")
		return
	}
	metrics, err := s.operations.Metrics(r.Context())
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) writeOperationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, operationstore.ErrInvalidRequest):
		writeProblem(w, r, http.StatusBadRequest, "https://ocservia.dev/problems/invalid-request", "Request is invalid", "the operation request is invalid")
	case errors.Is(err, operationstore.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/idempotency-conflict", "Idempotency conflict", "the Idempotency-Key was already used with different input")
	case errors.Is(err, operationstore.ErrStaleRevision):
		writeProblem(w, r, http.StatusConflict, "https://ocservia.dev/problems/stale-revision", "Resource revision is stale", "the node changed after this operation was prepared")
	case errors.Is(err, operationstore.ErrNodeUnavailable), errors.Is(err, pgx.ErrNoRows):
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested node or operation does not exist")
	default:
		s.logger.ErrorContext(r.Context(), "operation request failed", "error", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is unavailable", "operation state is temporarily unavailable")
	}
}

func expectedRevision(ifMatch string, explicit *int64) (int64, bool) {
	var header int64
	if ifMatch != "" {
		if len(ifMatch) < 3 || ifMatch[0] != '"' || ifMatch[len(ifMatch)-1] != '"' {
			return 0, false
		}
		value := ifMatch[1 : len(ifMatch)-1]
		if strings.ContainsRune(value, '"') {
			return 0, false
		}
		if !strings.HasPrefix(value, "revision-") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "revision-"), 10, 64)
		if err != nil || parsed < 1 {
			return 0, false
		}
		header = parsed
	}
	if explicit != nil && *explicit < 1 {
		return 0, false
	}
	if explicit != nil && header != 0 && *explicit != header {
		return 0, false
	}
	if explicit != nil {
		return *explicit, true
	}
	return header, header > 0
}
