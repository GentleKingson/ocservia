//go:build integration

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
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

func TestDatabaseCoreSmoke(t *testing.T) {
	ownerURL, runtimeURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"), os.Getenv("OCSERV_TEST_DATABASE_URL")
	if ownerURL == "" || runtimeURL == "" {
		t.Skip("isolated PostgreSQL owner/runtime credentials required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dir := testutil.PrivateTempDir(t)
	binary := filepath.Join(dir, "ocserv-control")
	if out, err := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", binary, "../../../cmd/ocserv-control").CombinedOutput(); err != nil {
		t.Fatalf("build Controller: %v\n%s", err, out)
	}

	envFor := func(dsn string, extra map[string]string) []string {
		vals := map[string]string{
			"OCSERV_ENVIRONMENT":              "test",
			"OCSERV_DATABASE_URL":             dsn,
			"OCSERV_AUDIT_EVENT_KEY_ID":       "test-audit-event-v1",
			"OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32),
			"OCSERV_AUDIT_CHECKPOINT_KEY":     strings.Repeat("22", 32),
			"OCSERV_SESSION_KEY":              strings.Repeat("33", 32),
			"OCSERV_LOCAL_AUTH_ENABLED":       "true",
			"OCSERV_RUNTIME_DATABASE_ROLE":    "ocservia_app",
		}
		for k, v := range extra {
			vals[k] = v
		}
		env := append([]string(nil), os.Environ()...)
		for k, v := range vals {
			env = append(env, k+"="+v)
		}
		return env
	}

	if out, err := exec.CommandContext(ctx, binary, "--migrate-only").CombinedOutput(); err == nil {
		_ = out // ensure env-configured run below works
	}
	migrate := exec.CommandContext(ctx, binary, "--migrate-only")
	migrate.Env = envFor(ownerURL, nil)
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("owner migrate: %v\n%s", err, out)
	}

	runtime, err := connection.Open(ctx, connection.Options{Environment: "test", URL: runtimeURL})
	testutil.NoError(t, err)
	defer runtime.Close()
	if _, err := runtime.Store.Exec(ctx, `CREATE TABLE startup_forbidden(id int)`); !errors.Is(err, database.ErrPermission) {
		t.Fatalf("runtime DDL was not denied: %v", err)
	}

	prov := exec.CommandContext(ctx, binary, "--provision-management-workspace")
	prov.Env = envFor(ownerURL, nil)
	provOut, err := prov.CombinedOutput()
	if err != nil {
		t.Fatalf("provision workspace: %v\n%s", err, provOut)
	}
	var workspace uuid.UUID
	for _, line := range bytes.Split(provOut, []byte("\n")) {
		var rec struct {
			Msg         string    `json:"msg"`
			WorkspaceID uuid.UUID `json:"workspace_id"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Msg == "management workspace ready" {
			workspace = rec.WorkspaceID
			break
		}
	}
	if workspace == uuid.Nil {
		t.Fatalf("missing workspace ID in output:\n%s", provOut)
	}

	checkSmokeTransactions(t, ctx, runtime.Store, workspace)

	pwFile := filepath.Join(dir, "admin-password")
	testutil.NoError(t, os.WriteFile(pwFile, []byte("uncommon setup password for controller administrator"), 0o600))
	boot := exec.CommandContext(ctx, binary, "--bootstrap-local-admin")
	boot.Env = envFor(runtimeURL, map[string]string{
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME":      "process-admin",
		"OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE": pwFile,
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID":  workspace.String(),
	})
	if out, err := boot.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap local admin: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := exec.CommandContext(ctx, binary)
	srv.Env = envFor(runtimeURL, map[string]string{
		"OCSERV_HTTP_ADDRESS":  addr,
		"OCSERV_PUBLIC_ORIGIN": "http://" + addr,
	})
	testutil.NoError(t, srv.Start())
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		_ = srv.Wait()
	})

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + addr
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if resp, err := client.Get(base + "/readyz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/login",
		strings.NewReader(`{"username":"process-admin","password":"uncommon setup password for controller administrator"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	resp, err := client.Do(req)
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(resp.Cookies()) != 1 {
		t.Fatalf("smoke login status=%d", resp.StatusCode)
	}
	wsReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/workspaces", nil)
	wsReq.AddCookie(resp.Cookies()[0])
	wsResp, err := client.Do(wsReq)
	testutil.NoError(t, err)
	body, _ := io.ReadAll(wsResp.Body)
	_ = wsResp.Body.Close()
	if wsResp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("committed workspace")) {
		t.Fatalf("smoke workspace readback: %d %s", wsResp.StatusCode, body)
	}
}

func checkSmokeTransactions(t *testing.T, ctx context.Context, store *postgres.Backend, workspace uuid.UUID) {
	t.Helper()
	rollback := errors.New("rollback smoke transaction")
	for _, commit := range []bool{true, false} {
		name := "committed workspace"
		if !commit {
			name = "rolled back workspace"
		}
		err := database.Within(ctx, store, database.ReadCommitted, func(tx database.Tx) error {
			q := tx.(interface {
				Exec(context.Context, string, ...any) (int64, error)
			})
			if _, err := q.Exec(ctx, `UPDATE workspaces SET name=$1 WHERE id=$2`, name, workspace); err != nil {
				return err
			}
			if !commit {
				return rollback
			}
			return nil
		})
		if commit && err != nil || !commit && !errors.Is(err, rollback) {
			t.Fatal(err)
		}
	}
	var got string
	if err := store.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspace).Scan(&got); err != nil || got != "committed workspace" {
		t.Fatalf("commit/rollback readback: %q %v", got, err)
	}
}
