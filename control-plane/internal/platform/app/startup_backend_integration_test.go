package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

func controllerProcessDatabase(t *testing.T) (owner, runtime connection.Options, account string) {
	t.Helper()
	ctx := context.Background()
	if dsn := os.Getenv("PR02_DSN"); dsn != "" {
		options := mysql.Options{Engine: mysql.Engine(os.Getenv("PR02_ENGINE")), Environment: "test", DSN: dsn}
		admin, err := mysql.Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
		name, user := "pr02_startup_"+suffix, "startup_"+suffix[:20]
		if _, err := admin.Exec(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE `"+name+"`") })
		if _, err := admin.Exec(ctx, "CREATE USER '"+user+"'@'%' IDENTIFIED BY 'startup-runtime-test-only'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP USER '"+user+"'@'%'") })
		if _, err := admin.Exec(ctx, "GRANT ALL ON `"+name+"`.* TO 'ocservia_owner'@'%' WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		cfg, err := driver.ParseDSN(dsn)
		if err != nil {
			t.Fatal("invalid fixture DSN")
		}
		cfg.DBName, cfg.User, cfg.Passwd = name, "ocservia_owner", "pr02-owner-test-only"
		owner = connection.Options{Backend: string(options.Engine), Environment: "test", URL: cfg.FormatDSN()}
		cfg.User, cfg.Passwd = user, "startup-runtime-test-only"
		runtime = owner
		runtime.URL = cfg.FormatDSN()
		return owner, runtime, user + "@%"
	}
	ownerURL, runtimeURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"), os.Getenv("OCSERV_TEST_DATABASE_URL")
	if ownerURL == "" || runtimeURL == "" {
		t.Skip("isolated real database with separate owner/runtime credentials required")
	}
	return connection.Options{Backend: "postgres", Environment: "test", URL: ownerURL}, connection.Options{Backend: "postgres", Environment: "test", URL: runtimeURL}, "ocservia_app"
}

// This starts the actual CLI binary, not an httptest router or a replacement
// application constructor. Agent/transport/certificate E2E is a separate gate.
func TestControllerProcessStartupBackendIntegration(t *testing.T) {
	ownerOptions, runtimeOptions, account := controllerProcessDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "ocserv-control")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "../../../cmd/ocserv-control")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Controller: %v\n%s", err, output)
	}
	environment := func(options connection.Options, extra map[string]string) []string {
		var env []string
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "OCSERV_") && !strings.HasPrefix(item, "OTEL_") && !strings.HasPrefix(item, "PR02_") {
				env = append(env, item)
			}
		}
		values := map[string]string{
			"OCSERV_ENVIRONMENT":               "test",
			"OCSERV_DATABASE_BACKEND":          options.Backend,
			"OCSERV_DATABASE_URL":              options.URL,
			"OCSERV_AUDIT_EVENT_KEY_ID":        "test-audit-event-v1",
			"OCSERV_TEST_AUDIT_EVENT_KEY_HEX":  strings.Repeat("11", 32),
			"OCSERV_AUDIT_CHECKPOINT_KEY":      strings.Repeat("22", 32),
			"OCSERV_SESSION_KEY":               strings.Repeat("33", 32),
			"OCSERV_LOCAL_AUTH_ENABLED":        "true",
			"OCSERV_RECOMMENDED_AGENT_VERSION": "1.2.3",
		}
		for key, v := range extra {
			values[key] = v
		}
		for key, v := range values {
			env = append(env, key+"="+v)
		}
		return env
	}
	run := func(options connection.Options, extra map[string]string, args ...string) error {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = environment(options, extra)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(string(output))
		}
		return nil
	}
	if err := run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err != nil {
		t.Fatal("owner CLI migration", err)
	}
	if err := run(runtimeOptions, nil, "--schema-compatibility-check=35"); err != nil {
		t.Fatal("runtime CLI schema validation", err)
	}
	if err := run(runtimeOptions, nil, "--schema-compatibility-check=999"); err == nil {
		t.Fatal("unsupported Controller schema accepted")
	}
	if err := run(runtimeOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err == nil {
		t.Fatal("runtime account performed owner migration")
	}
	if err := run(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}, "--migrate-only"); err != nil {
		t.Fatal("idempotent owner CLI migration", err)
	}
	owner, err := connection.Open(ctx, ownerOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	runtime, err := connection.Open(ctx, runtimeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtimeOptions.Backend != "postgres" {
		t.Run("roles-refuse-missing-telemetry-month", func(t *testing.T) {
			var month []byte
			if err := owner.Store.QueryRow(ctx, `SELECT table_name FROM telemetry_sample_shards WHERE state='active' AND start_at<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)) AND end_at>TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))`).Scan(&month); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Store.Exec(ctx, `UPDATE telemetry_sample_shards SET state='retired' WHERE table_name=?`, month); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := owner.Store.Exec(ctx, `UPDATE telemetry_sample_shards SET state='active' WHERE table_name=?`, month); err != nil {
					t.Error(err)
				}
			}()
			for _, role := range []string{"api", "worker", "scheduler", "all"} {
				t.Run(role, func(t *testing.T) {
					checkCtx, stop := context.WithTimeout(ctx, 10*time.Second)
					defer stop()
					cmd := exec.CommandContext(checkCtx, binary, "--role="+role)
					cmd.Env = environment(runtimeOptions, nil)
					output, err := cmd.CombinedOutput()
					if err == nil || !bytes.Contains(output, []byte("validate required runtime database capabilities")) {
						t.Fatalf("role did not refuse missing telemetry storage: %v\n%s", err, output)
					}
				})
			}
		})
	}
	if _, err := runtime.Store.Exec(ctx, `CREATE TABLE startup_forbidden(id int)`); !errors.Is(err, database.ErrPermission) {
		t.Fatal("runtime DDL was not denied", err)
	}
	workspace, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	at, _ := value.FromTime(time.Now().UTC())
	query := `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'Controller process',$2,$3,$3)`
	var workspaceArg any = workspace
	if runtimeOptions.Backend != "postgres" {
		query = `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'Controller process',?,?,?)`
		workspaceArg = mysql.UUIDBytes(workspace)
	}
	args := []any{workspaceArg, workspace.String(), at}
	if runtimeOptions.Backend != "postgres" {
		args = append(args, at)
	}
	if _, err := runtime.Store.Exec(ctx, query, args...); err != nil {
		t.Fatal("seed workspace", err)
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
	installSchedulerEvidence(t, ctx, owner, runtimeOptions.Backend, account)
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
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = environment(runtimeOptions, map[string]string{
		"OCSERV_HTTP_ADDRESS": address, "OCSERV_PUBLIC_ORIGIN": "http://" + address,
		"OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "true",
	})
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
	request.Header.Set("Origin", base)
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
	completed := 0
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if err := owner.Store.QueryRow(ctx, `SELECT count(*) FROM g6_scheduler_maintenance_history`).Scan(&completed); err != nil {
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
}

func installSchedulerEvidence(t *testing.T, ctx context.Context, owner *connection.Connection, backend, account string) {
	t.Helper()
	if backend == "postgres" {
		sql, err := os.ReadFile("../../../../scripts/g6-authority-history.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Store.Exec(ctx, string(sql)); err != nil {
			t.Fatal("install PostgreSQL evidence fixture", err)
		}
		return
	}
	// Test-only owner objects are installed after immutable schema validation.
	// Runtime gets EXECUTE on the exact-term recorder, never journal INSERT.
	for _, sql := range []string{
		`CREATE TABLE g6_scheduler_maintenance_history(maintenance_id BIGINT AUTO_INCREMENT PRIMARY KEY,instance_id VARBINARY(16) NOT NULL,incarnation BIGINT NOT NULL,epoch BIGINT NOT NULL,completed_at BIGINT NOT NULL) ENGINE=InnoDB`,
		`CREATE PROCEDURE g6_record_scheduler_maintenance(IN requested_instance_id VARBINARY(16),IN requested_incarnation BIGINT,IN requested_epoch BIGINT)
SQL SECURITY DEFINER
BEGIN
 DECLARE valid_until BIGINT DEFAULT NULL;
 SELECT lease_until INTO valid_until FROM scheduler_leadership WHERE id=1 AND instance_id=requested_instance_id AND incarnation=requested_incarnation AND epoch=requested_epoch LOCK IN SHARE MODE;
 IF valid_until IS NULL OR valid_until<=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='scheduler maintenance term is not the exact live leader';
 END IF;
 INSERT INTO g6_scheduler_maintenance_history(instance_id,incarnation,epoch,completed_at) VALUES(requested_instance_id,requested_incarnation,requested_epoch,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6)));
END`,
	} {
		if _, err := owner.Store.Exec(ctx, sql); err != nil {
			t.Fatal("install MySQL evidence fixture", err)
		}
	}
	user, _, _ := strings.Cut(account, "@")
	if _, err := owner.Store.Exec(ctx, "GRANT EXECUTE ON PROCEDURE g6_record_scheduler_maintenance TO '"+user+"'@'%'"); err != nil {
		t.Fatal(err)
	}
}
