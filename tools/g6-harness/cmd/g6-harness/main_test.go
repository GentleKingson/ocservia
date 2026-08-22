package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
)

func TestWaitCommandPersistsStructuredFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("GITHUB_SHA", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("GITHUB_RUN_ID", "424242")
	t.Setenv("GITHUB_RUN_ATTEMPT", "3")
	t.Setenv("GITHUB_REPOSITORY", "ocservia/example")
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("G6_AUTHORITY", "engineering")
	t.Setenv("G6_RENDEZVOUS_MAX_CONSECUTIVE_ERRORS", "1")
	root := t.TempDir()
	resultPath := filepath.Join(root, "result.json")
	err := run([]string{
		"wait-download",
		"--name", "g6-rd-tunnel-fd-b-424242-3",
		"--destination", filepath.Join(root, "destination"),
		"--peer-job", "G6 Readiness Failure Domain B",
		"--timeout", "1s",
		"--result", resultPath,
		"--state", filepath.Join(root, "state.json"),
	})
	if err == nil {
		t.Fatal("404 wait unexpectedly succeeded")
	}
	content, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var result rendezvous.Result
	if decodeErr := json.Unmarshal(content, &result); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if result.SchemaVersion != rendezvous.ResultSchemaVersion || result.Status != "failed" ||
		result.Failure == nil || result.Failure.Code != "artifact_api_unavailable" || result.CompletedAt.IsZero() {
		t.Fatalf("unexpected structured result: %+v", result)
	}
}
