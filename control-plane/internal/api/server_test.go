package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLiveAndRequestID(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{Version: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false)
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request ID")
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestDevAuthMarker(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, true)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Header().Get("X-Ocservia-Dev-Subject") != "developer" {
		t.Fatal("development subject was not injected")
	}
}

func TestVersionUsesContractFieldNames(t *testing.T) {
	server := New("127.0.0.1:0", nil, BuildInfo{Version: "test", Commit: "abc", Role: "api"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false)
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/version", nil))
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["version"] != "test" || body["Version"] != "" {
		t.Fatalf("unexpected response: %v", body)
	}
}
