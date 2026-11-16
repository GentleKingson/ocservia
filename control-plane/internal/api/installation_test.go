package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
)

func TestInstallationAdmissionBeforeDecode(t *testing.T) {
	server := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
	server.EnableEnrollment(enrollment.NewBackend(nil, "", "test", nil), nil)
	paths := []string{"/api/v1/node-bootstrap/config", "/api/v1/node-bootstrap/attestation", "/api/v1/node-bootstrap/register", "/api/v1/node-bootstrap/status"}
	for i := 0; i < 31; i++ {
		request := httptest.NewRequest(http.MethodPost, paths[i%len(paths)], strings.NewReader(`{"token":`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		want := http.StatusBadRequest
		if i == 30 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("attempt %d: %d %s", i, response.Code, response.Body)
		}
		if i == 30 && response.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry backoff")
		}
	}
}

func TestInstallationConfigRejectsChildWithoutDisclosure(t *testing.T) {
	server := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, time.Second, false, "")
	server.EnableEnrollment(enrollment.NewBackend(nil, "", "test", nil), nil)
	secret := "obt2_" + strings.Repeat("a", 43)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/node-bootstrap/config", strings.NewReader(`{"token":"`+secret+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), secret) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("child token response: %d %s", response.Code, response.Body)
	}
}
