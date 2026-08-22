package rendezvous

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitDownloadSuccess(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	archive := checkpointArchive(t, name, testBinding(), "fd-b", nil)
	server := checkpointServer(t, name, archive, hexDigest(archive), 1, http.StatusOK)
	defer server.Close()
	options := testOptions(t, server.URL, name)
	result, err := WaitDownload(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "passed" || result.Artifact == nil || result.Artifact.ID != 99 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if value, err := os.ReadFile(filepath.Join(options.Destination, "candidate-sha")); err != nil || string(value) != "candidate\n" {
		t.Fatalf("downloaded payload mismatch: %q, %v", value, err)
	}
}

func TestWaitDownloadRejectsHTTP404(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	options := testOptions(t, server.URL, "g6-rd-agents-enrolled-fd-b-424242-3")
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "artifact_api_unavailable")
}

func TestWaitDownloadRejectsPersistentHTTP5xx(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	options := testOptions(t, server.URL, "g6-rd-agents-enrolled-fd-b-424242-3")
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "artifact_api_unavailable")
}

func TestWaitDownloadTimesOut(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "/artifacts") {
			fmt.Fprint(response, `{"total_count":0,"artifacts":[]}`)
			return
		}
		fmt.Fprint(response, `{"jobs":[{"name":"G6 Readiness Core / G6 Readiness Failure Domain B","status":"in_progress","conclusion":"","steps":[]}]}`)
	}))
	defer server.Close()
	options := testOptions(t, server.URL, name)
	options.Timeout = 20 * time.Millisecond
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "checkpoint_wait_timeout")
}

func TestWaitDownloadRejectsDuplicateArtifact(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	archive := checkpointArchive(t, name, testBinding(), "fd-b", nil)
	server := checkpointServer(t, name, archive, hexDigest(archive), 2, http.StatusOK)
	defer server.Close()
	options := testOptions(t, server.URL, name)
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "duplicate_artifact")
}

func TestWaitDownloadRejectsArtifactDigestMismatch(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	archive := checkpointArchive(t, name, testBinding(), "fd-b", nil)
	server := checkpointServer(t, name, archive, strings.Repeat("a", 64), 1, http.StatusOK)
	defer server.Close()
	options := testOptions(t, server.URL, name)
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "artifact_digest_mismatch")
}

func TestWaitDownloadRecoversFromBoundedDownload5xx(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	archive := checkpointArchive(t, name, testBinding(), "fd-b", nil)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/artifacts/99/zip"):
			if attempts.Add(1) < 3 {
				http.Error(response, "temporary", http.StatusServiceUnavailable)
				return
			}
			response.Write(archive)
		case strings.Contains(request.URL.Path, "/artifacts"):
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(response, `{"total_count":1,"artifacts":[{"id":99,"name":%q,"expired":false,"digest":%q}]}`, name, "sha256:"+hexDigest(archive))
		default:
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprint(response, `{"jobs":[{"name":"G6 Readiness Core / G6 Readiness Failure Domain B","status":"in_progress","conclusion":"","steps":[]}]}`)
		}
	}))
	defer server.Close()
	options := testOptions(t, server.URL, name)
	options.DownloadRetryTotal = 100 * time.Millisecond
	if _, err := WaitDownload(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected three bounded download attempts, got %d", attempts.Load())
	}
}

func TestWaitDownloadRejectsWrongManifestBinding(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	wrong := testBinding()
	wrong.CandidateSHA = "1123456789abcdef0123456789abcdef01234567"
	archive := checkpointArchive(t, name, wrong, "fd-b", nil)
	server := checkpointServer(t, name, archive, hexDigest(archive), 1, http.StatusOK)
	defer server.Close()
	options := testOptions(t, server.URL, name)
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "checkpoint_manifest_rejected")
}

func TestWaitDownloadRejectsZipPathTraversal(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	member, err := writer.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write([]byte("escape")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buffer.Bytes()
	server := checkpointServer(t, name, archive, hexDigest(archive), 1, http.StatusOK)
	defer server.Close()
	options := testOptions(t, server.URL, name)
	_, err = WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "artifact_path_unsafe")
}

func TestWaitDownloadRejectsPeerFailureBeforeDownload(t *testing.T) {
	t.Parallel()
	name := "g6-rd-agents-enrolled-fd-b-424242-3"
	archive := checkpointArchive(t, name, testBinding(), "fd-b", nil)
	downloaded := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/artifacts/"):
			downloaded = true
			response.Write(archive)
		case strings.Contains(request.URL.Path, "/artifacts"):
			fmt.Fprintf(response, `{"total_count":1,"artifacts":[{"id":99,"name":%q,"expired":false,"digest":%q}]}`, name, "sha256:"+hexDigest(archive))
		default:
			fmt.Fprint(response, `{"jobs":[{"name":"G6 Readiness Core / G6 Readiness Failure Domain B","status":"in_progress","conclusion":"","steps":[{"name":"enroll","status":"completed","conclusion":"failure"}]}]}`)
		}
	}))
	defer server.Close()
	options := testOptions(t, server.URL, name)
	_, err := WaitDownload(context.Background(), options)
	assertFailureCode(t, err, "peer_step_failed")
	if downloaded {
		t.Fatal("artifact downloaded before peer failure was rejected")
	}
}

func TestAdvanceStateRejectsDuplicateAndSequenceRollback(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "state.json")
	high := Manifest{SchemaVersion: CheckpointSchemaVersion, ProducerDomain: "fd-b", Checkpoint: "promotion-complete", Sequence: 120}
	if err := advanceState(statePath, high); err != nil {
		t.Fatal(err)
	}
	if err := advanceState(statePath, high); err == nil {
		t.Fatal("duplicate checkpoint was accepted")
	}
	low := Manifest{SchemaVersion: CheckpointSchemaVersion, ProducerDomain: "fd-b", Checkpoint: "load-active", Sequence: 90}
	if err := advanceState(statePath, low); err == nil {
		t.Fatal("checkpoint sequence rollback was accepted")
	}
}

func testOptions(t *testing.T, baseURL, name string) Options {
	t.Helper()
	root := t.TempDir()
	return Options{
		BaseURL:              baseURL,
		Repository:           "ocservia/example",
		Token:                "test-token",
		Binding:              testBinding(),
		ArtifactName:         name,
		Destination:          filepath.Join(root, "destination"),
		PeerJob:              "G6 Readiness Core / G6 Readiness Failure Domain B",
		StatePath:            filepath.Join(root, "state.json"),
		Timeout:              250 * time.Millisecond,
		PollInterval:         time.Millisecond,
		PropagationGrace:     20 * time.Millisecond,
		RequestTimeout:       50 * time.Millisecond,
		DownloadRetryTotal:   20 * time.Millisecond,
		MaxConsecutiveErrors: 2,
	}
}

func checkpointArchive(t *testing.T, name string, binding Binding, producer string, mutate func(string)) []byte {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "candidate-sha"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateManifest(root, name, producer, binding, time.Now().UTC(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(root)
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		member, err := writer.Create(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func checkpointServer(t *testing.T, name string, archive []byte, digest string, totalCount, metadataStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/artifacts/99/zip"):
			response.Write(archive)
		case strings.Contains(request.URL.Path, "/artifacts"):
			response.Header().Set("Content-Type", "application/json")
			if metadataStatus != http.StatusOK {
				response.WriteHeader(metadataStatus)
				return
			}
			artifacts := []map[string]any{{"id": 99, "name": name, "expired": false, "digest": "sha256:" + digest}}
			if totalCount > 1 {
				artifacts = append(artifacts, map[string]any{"id": 100, "name": name, "expired": false, "digest": "sha256:" + digest})
			}
			json.NewEncoder(response).Encode(map[string]any{"total_count": totalCount, "artifacts": artifacts})
		default:
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprint(response, `{"jobs":[{"name":"G6 Readiness Core / G6 Readiness Failure Domain B","status":"in_progress","conclusion":"","steps":[]}]}`)
		}
	}))
}

func hexDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func assertFailureCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected failure %s", code)
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("expected structured failure, got %T: %v", err, err)
	}
	if failure.Code != code {
		t.Fatalf("expected failure %s, got %+v", code, failure)
	}
}
