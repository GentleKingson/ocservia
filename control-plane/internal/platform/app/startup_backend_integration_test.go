package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func controllerProcessDatabase(t *testing.T) (owner, runtime connection.Options, account string) {
	t.Helper()
	ownerURL, runtimeURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"), os.Getenv("OCSERV_TEST_DATABASE_URL")
	if ownerURL == "" || runtimeURL == "" {
		t.Skip("isolated real PostgreSQL database with separate owner/runtime credentials required")
	}
	return connection.Options{Backend: "postgres", Environment: "test", URL: ownerURL}, connection.Options{Backend: "postgres", Environment: "test", URL: runtimeURL}, "ocservia_app"
}

// This starts the actual CLI binary, not an httptest router or a replacement
// application constructor. Agent/transport/certificate E2E is a separate gate.
func TestControllerProcessStartupBackendIntegration(t *testing.T) {
	controllerProcessCheck(t, false)
}

func TestDatabaseCoreSmoke(t *testing.T) {
	controllerProcessCheck(t, true)
}

func controllerProcessCheck(t *testing.T, smoke bool) {
	ownerOptions, runtimeOptions, account := controllerProcessDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// Production secrets reject a world-writable TMPDIR ancestor (including
	// /tmp on CI). Keep this fixture under the private repository ancestry.
	dir := socketDirectory(t)
	production := runtimeOptions.Environment == "production"
	auditEventKeyFile, commandSigningKeyFile := "", ""
	if production {
		auditEventKeyFile = filepath.Join(dir, "audit-event-key")
		if err := os.WriteFile(auditEventKeyFile, []byte(strings.Repeat("11", 32)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		commandSigningKeyFile = filepath.Join(dir, "controller-command-signing-key.pem")
		if err := os.WriteFile(commandSigningKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Only the database script supplies a binary freshly built for this test.
	binary := os.Getenv("OCSERVIA_TEST_STARTUP_BIN")
	if binary != "" {
		info, err := os.Stat(binary)
		if err != nil || !filepath.IsAbs(binary) || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("OCSERVIA_TEST_STARTUP_BIN must name an absolute executable file: %v", err)
		}
	} else {
		binary = filepath.Join(dir, "ocserv-control")
		build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "../../../cmd/ocserv-control")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build Controller: %v\n%s", err, output)
		}
	}
	environment := func(options connection.Options, extra map[string]string) []string {
		var env []string
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "OCSERV_") && !strings.HasPrefix(item, "OTEL_") && !strings.HasPrefix(item, "OCSERVIA_DATABASE_TEST_") {
				env = append(env, item)
			}
		}
		values := map[string]string{
			"OCSERV_ENVIRONMENT":               options.Environment,
			"OCSERV_DATABASE_BACKEND":          options.Backend,
			"OCSERV_DATABASE_URL":              options.URL,
			"OCSERV_AUDIT_EVENT_KEY_ID":        "test-audit-event-v1",
			"OCSERV_TEST_AUDIT_EVENT_KEY_HEX":  strings.Repeat("11", 32),
			"OCSERV_AUDIT_CHECKPOINT_KEY":      strings.Repeat("22", 32),
			"OCSERV_SESSION_KEY":               strings.Repeat("33", 32),
			"OCSERV_LOCAL_AUTH_ENABLED":        "true",
			"OCSERV_RECOMMENDED_AGENT_VERSION": "1.2.3",
		}
		if options.CAFile != "" {
			values["OCSERV_DATABASE_TLS_CA_FILE"] = options.CAFile
		}
		if production {
			delete(values, "OCSERV_TEST_AUDIT_EVENT_KEY_HEX")
			values["OCSERV_AUDIT_EVENT_KEY_ID"] = "production-e2e-audit-v1"
			values["OCSERV_AUDIT_EVENT_KEY_FILE"] = auditEventKeyFile
			values["OCSERV_PUBLIC_ORIGIN"] = "https://controller.example.test"
			values["OCSERV_COMMAND_SIGNING_KEY_FILE"] = commandSigningKeyFile
			values["OCSERV_CONTROLLER_ENDPOINT_ID"] = strings.Repeat("ab", 32)
			values["OCSERV_TRANSPORT_UID"] = strconv.Itoa(os.Geteuid() + 1)
			values["OCSERV_TRANSPORT_GID"] = strconv.Itoa(os.Getegid())
		}
		for key, v := range extra {
			values[key] = v
		}
		for key, v := range values {
			env = append(env, key+"="+v)
		}
		return env
	}
	runOutput := func(options connection.Options, extra map[string]string, args ...string) ([]byte, error) {
		t.Helper()
		// Every invocation here is one-shot. Invalid role-only resources must
		// never be constructed, even with the default all role and an endpoint.
		oneShot := map[string]string{
			"OCSERV_COMMAND_SIGNING_KEY_FILE": filepath.Join(dir, "missing-role-key"),
			"OCSERV_CONTROLLER_ENDPOINT_ID":   strings.Repeat("ab", 32),
			"OCSERV_TRUST_SOCKET":             filepath.Join(dir, "unused-trust.sock"),
		}
		for key, value := range extra {
			oneShot[key] = value
		}
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = environment(options, oneShot)
		output, err := cmd.CombinedOutput()
		if _, statErr := os.Stat(oneShot["OCSERV_TRUST_SOCKET"]); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("one-shot command created a Trust socket", statErr)
		}
		return output, err
	}
	run := func(options connection.Options, extra map[string]string, args ...string) error {
		t.Helper()
		if output, err := runOutput(options, extra, args...); err != nil {
			return errors.New(string(output))
		}
		return nil
	}
	if err := run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err != nil {
		t.Fatal("owner CLI migration", err)
	}
	if !smoke {
		if err := run(runtimeOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err == nil {
			t.Fatal("runtime account performed owner migration")
		}
		if err := run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err != nil {
			t.Fatal("idempotent owner CLI migration", err)
		}
	}
	owner, err := connection.Open(ctx, ownerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	runtime, err := connection.Open(ctx, runtimeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if !smoke {
		t.Run("cleanup-privilege-repair", func(t *testing.T) {
			checkCleanupCLIRecovery(t, ctx, owner, runtime, account, func() error {
				return run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only")
			})
		})
	}
	if _, err := runtime.Store.Exec(ctx, `CREATE TABLE startup_forbidden(id int)`); !errors.Is(err, database.ErrPermission) {
		t.Fatal("runtime DDL was not denied", err)
	}
	// Smoke databases start empty, so the owner one-shot creates the
	// management workspace and a rerun returns the same one.
	provision := func() (id uuid.UUID, created bool) {
		t.Helper()
		output, err := runOutput(ownerOptions, nil, "--provision-management-workspace")
		if err != nil {
			t.Fatalf("provision management workspace: %v\n%s", err, output)
		}
		for _, line := range bytes.Split(output, []byte("\n")) {
			var record struct {
				Msg         string    `json:"msg"`
				WorkspaceID uuid.UUID `json:"workspace_id"`
				Created     bool      `json:"created"`
			}
			if json.Unmarshal(line, &record) == nil && record.Msg == "management workspace ready" {
				return record.WorkspaceID, record.Created
			}
		}
		t.Fatalf("workspace provisioning did not report the workspace:\n%s", output)
		return
	}
	var workspace uuid.UUID
	if smoke {
		var created bool
		if workspace, created = provision(); !created || workspace.Version() != 7 {
			t.Fatal("first provisioning", workspace, created)
		}
		if again, created := provision(); again != workspace || created {
			t.Fatal("repeat provisioning", again, created)
		}
	} else if workspace, err = uuid.NewV7(); err != nil {
		t.Fatal(err)
	}
	at, _ := value.FromTime(time.Now().UTC())
	if !smoke {
		if _, err := runtime.Store.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Controller process',$2,$3,$3)`, workspace, workspace.String(), at); err != nil {
			t.Fatal("seed workspace", err)
		}
	} else {
		checkSmokeTransactions(t, ctx, runtime.Store, workspace)
		if err := run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err != nil {
			t.Fatal("repeat migration with business data", err)
		}
		var name string
		if err := runtime.Store.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspace).Scan(&name); err != nil || name != "committed workspace" {
			t.Fatal("repeat migration changed committed data", name, err)
		}
	}
	password := "uncommon setup password for controller administrator"
	adminFile, approverFile := filepath.Join(dir, "admin-password"), filepath.Join(dir, "approver-password")
	for path, secret := range map[string]string{adminFile: password, approverFile: "separate long setup password for controller approver"} {
		if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap := map[string]string{
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME":               "process-admin",
		"OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE":          adminFile,
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID":           workspace.String(),
		"OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME":      "process-approver",
		"OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE": approverFile,
	}
	if err := run(runtimeOptions, bootstrap, "--bootstrap-local-admin"); err != nil {
		t.Fatal("runtime CLI bootstrap", err)
	}
	// A rerun reports the initialized state distinctly so installers can
	// resume after an interruption between commit and their own marker.
	if output, err := runOutput(runtimeOptions, bootstrap, "--bootstrap-local-admin"); err == nil ||
		!bytes.Contains(output, []byte("Local authentication is already initialized")) {
		t.Fatalf("repeat runtime CLI bootstrap: %v\n%s", err, output)
	}
	if !production && !smoke {
		installSchedulerEvidence(t, ctx, owner)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	logPath := filepath.Join(dir, "controller.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	processArgs := []string(nil)
	extra := map[string]string{"OCSERV_HTTP_ADDRESS": address}
	if production || smoke {
		processArgs = append(processArgs, "--role=api")
	}
	if !production {
		extra["OCSERV_PUBLIC_ORIGIN"] = "http://" + address
		if !smoke {
			extra["OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE"] = "true"
		}
	}
	cmd := exec.CommandContext(ctx, binary, processArgs...)
	cmd.Env = environment(runtimeOptions, extra)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + address
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-done:
			finished = true
			output, _ := os.ReadFile(logPath)
			t.Fatalf("Controller stopped before readiness: %v\n%s", err, output)
		default:
		}
		response, err := client.Get(base + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("Controller never became ready\n%s", output)
	}
	payload, _ := json.Marshal(map[string]string{"username": "process-admin", "password": password})
	request, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/v1/auth/login", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	origin := base
	if production {
		origin = "https://controller.example.test"
	}
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(response.Cookies()) != 1 {
		t.Fatalf("real process login: %d %s", response.StatusCode, body)
	}
	cookie := response.Cookies()[0]
	request, _ = http.NewRequestWithContext(ctx, "GET", base+"/api/v1/nodes", nil)
	request.AddCookie(cookie)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !json.Valid(body) {
		t.Fatalf("real process authorized reader: %d %s", response.StatusCode, body)
	}
	if !production && !smoke {
		completed := 0
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
			if err := owner.Store.QueryRow(ctx, `SELECT count(*) FROM test_scheduler_maintenance_history`).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if completed > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if completed == 0 {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("real maintenance body did not finish with its fence\n%s", output)
		}
	}
	if production {
		t.Log("production configuration, migration, runtime permissions, readiness, authenticated read, and database writes passed")
	}
	if smoke {
		request, _ = http.NewRequestWithContext(ctx, "GET", base+"/api/v1/workspaces", nil)
		request.AddCookie(cookie)
		response, err = client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("committed workspace")) {
			t.Fatalf("authenticated workspace readback: %d %s", response.StatusCode, body)
		}
		return
	}
	streamRequest, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/events/stream", nil)
	streamRequest.AddCookie(cookie)
	streamRequest.Header.Set("X-Workspace-ID", workspace.String())
	streamClient := &http.Client{}
	defer streamClient.CloseIdleConnections()
	stream, err := streamClient.Do(streamRequest)
	if err != nil {
		t.Fatal("open real process SSE", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE not active: %d", stream.StatusCode)
	}
	streamDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stream.Body); close(streamDone) }()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal("Controller graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Controller shutdown timed out")
	}
	select {
	case <-streamDone:
	case <-time.After(time.Second):
		t.Fatal("SSE survived process shutdown")
	}
	rebound, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("HTTP listener survived SIGTERM", err)
	}
	_ = rebound.Close()
	t.Run("HTTP-failure-exit", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		failed := exec.CommandContext(ctx, binary, "--role=all")
		trustSocket := filepath.Join(socketDirectory(t), "trust.sock")
		failed.Env = environment(runtimeOptions, map[string]string{
			"OCSERV_HTTP_ADDRESS":           listener.Addr().String(),
			"OCSERV_CONTROLLER_ENDPOINT_ID": strings.Repeat("ab", 32),
			"OCSERV_TRUST_SOCKET":           trustSocket,
		})
		output, err := failed.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(output, []byte("serve HTTP")) {
			t.Fatalf("real CLI failure was not exit 1: %v\n%s", err, output)
		}
		if _, err := os.Stat(trustSocket); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed CLI left Trust socket", err)
		}
	})
}

func installSchedulerEvidence(t *testing.T, ctx context.Context, owner *connection.Connection) {
	t.Helper()
	sql, err := os.ReadFile("../../../../deploy/test-fixtures/scheduler-maintenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Store.Exec(ctx, string(sql)); err != nil {
		t.Fatal("install PostgreSQL evidence fixture", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := owner.Store.Exec(cleanup, `DROP FUNCTION public.test_record_scheduler_maintenance(uuid,bigint,bigint); DROP TABLE public.test_scheduler_maintenance_history`); err != nil {
			t.Error("remove PostgreSQL evidence fixture", err)
		}
	})
}
