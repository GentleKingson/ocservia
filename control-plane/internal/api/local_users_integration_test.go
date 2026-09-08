package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestChangeLocalPasswordAuthorization(t *testing.T) {
	// The issuer guard must run before any service/database call.
	server := &Server{auth: &auth.Service{}}
	for _, actor := range []auth.Principal{
		{Issuer: "https://idp.example", IdentityID: uuid.New(), SessionID: uuid.New()},
		{Issuer: auth.LocalIssuer, BreakGlass: true, IdentityID: uuid.New(), SessionID: uuid.New()},
		{Issuer: "break-glass", BreakGlass: true, IdentityID: uuid.New(), SessionID: uuid.New()},
		{Issuer: auth.LocalIssuer, IdentityID: uuid.New(), SessionID: uuid.New()},
	} {
		_, err := server.authorizeRoute(httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", nil), actor)
		allowed := actor.Issuer == auth.LocalIssuer && !actor.BreakGlass
		if (err == nil) != allowed {
			t.Fatalf("issuer=%s breakGlass=%v: %v", actor.Issuer, actor.BreakGlass, err)
		}
	}
}

// Run against a freshly migrated disposable database, like the other DB suites.
func TestLocalUserLifecycleIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL")
	if ownerURL == "" {
		ownerURL = url
	}
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	svc, err := auth.New(ctx, pool, auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := uuid.Must(uuid.NewV7())
	otherWorkspace := uuid.Must(uuid.NewV7())
	defer func() {
		// Keep append-only audit history, but remove this fixture's singleton
		// and admin grants so a repeated test run can bootstrap again.
		_, _ = owner.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS p4_reject_bootstrap, DROP CONSTRAINT IF EXISTS p4_reject_create, DROP CONSTRAINT IF EXISTS p4_reject_reset`)
		_, _ = owner.Exec(ctx, `DELETE FROM local_auth_bootstrap WHERE workspace_id=$1`, workspaceID)
		_, _ = owner.Exec(ctx, `DELETE FROM role_bindings WHERE workspace_id IN ($1,$2)`, workspaceID, otherWorkspace)
	}()
	username := "bootstrap-" + uuid.NewString()
	const password = "p4-secret-not-for-logs"
	const approverPassword = "independent-p4-approver-secret"
	approverName := "approver-" + username
	bootstrap := func(name, secret string) (uuid.UUID, error) {
		return svc.BootstrapLocalAdmin(ctx, name, secret, workspaceID, approverName, approverPassword)
	}
	if _, err := bootstrap(username, password); err == nil {
		t.Fatal("bootstrap with missing workspace succeeded")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject=$1)+(SELECT count(*) FROM local_credentials WHERE username=$1)+(SELECT count(*) FROM local_auth_bootstrap)`, username).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bootstrap rollback: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Local lifecycle',$2,now(),now())`, workspaceID, username); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap(username, "ocserviapassword"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("weak bootstrap: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject=$1)+(SELECT count(*) FROM local_credentials WHERE username=$1)+(SELECT count(*) FROM local_auth_bootstrap)+(SELECT count(*) FROM role_bindings WHERE workspace_id=$2)`, username, workspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("weak bootstrap wrote rows: %d %v", count, err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT p4_reject_bootstrap CHECK (action <> 'local_user.bootstrap') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap(username, password); err == nil {
		t.Fatal("bootstrap committed despite audit failure")
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT p4_reject_bootstrap`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject=$1)+(SELECT count(*) FROM local_credentials WHERE username=$1)+(SELECT count(*) FROM local_auth_bootstrap)+(SELECT count(*) FROM role_bindings WHERE workspace_id=$2)`, username, workspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bootstrap audit rollback: %d %v", count, err)
	}
	adminID, err := bootstrap(username, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap(username, "must not replace password"); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	adminCookie, _, err := svc.AuthenticateLocal(ctx, username, password)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := New("", pool, BuildInfo{}, slog.New(slog.NewJSONHandler(&logs, nil)), 1<<20, time.Second*15, false, "", 32)
	server.EnableAuthorization(svc, rbac.New(pool), approvals.New(pool), nil)
	server.EnableBrowserOrigin("https://console.example")
	call := func(path, body string, cookie *http.Cookie, origin, approval string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Approval-ID", approval)
		r.Header.Set("X-Workspace-ID", workspaceID.String())
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
		if strings.Contains(w.Body.String(), password) {
			t.Fatal("password in response")
		}
	}
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, "member-"+username, password)
	weakPassword := "12345678901234567890"
	weakBody := fmt.Sprintf(`{"username":%q,"password":%q}`, "member-"+username, weakPassword)
	w := call("local-users", weakBody, adminCookie, "https://console.example", "")
	expect(w, 400)
	if !strings.Contains(w.Body.String(), "not common, compromised or service-related") || strings.Contains(w.Body.String(), weakPassword) {
		t.Fatal("unsafe or unclear password policy response")
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identities WHERE subject=$1)+(SELECT count(*) FROM local_credentials WHERE username=$1)`, "member-"+username).Scan(&count); err != nil || count != 0 {
		t.Fatalf("weak create wrote rows: %d %v", count, err)
	}
	expect(call("local-users", body, nil, "https://console.example", ""), 401)
	expect(call("local-users", body, adminCookie, "https://sibling.example", ""), 403)
	expect(call("local-users", body, adminCookie, "", ""), 403)
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT p4_reject_create CHECK (action <> 'local_user.create') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	expect(call("local-users", body, adminCookie, "https://console.example", ""), 500)
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT p4_reject_create`); err != nil {
		t.Fatal(err)
	}
	w = call("local-users", body, adminCookie, "https://console.example", "")
	expect(w, 201)
	var created struct {
		IdentityID uuid.UUID `json:"identity_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.IdentityID
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings WHERE identity_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("create granted roles: %d %v", count, err)
	}
	expect(call("local-users", body, adminCookie, "https://console.example", ""), 409)
	expect(call("local-users", fmt.Sprintf(`{"username":%q,"password":%q}`, strings.ToUpper("member-"+username), password), adminCookie, "https://console.example", ""), 409)
	memberCookie, _, err := svc.AuthenticateLocal(ctx, "member-"+username, password)
	if err != nil {
		t.Fatal(err)
	}
	selfPath := "auth/change-password"
	expect(call(selfPath, `{"current_password":"wrong-current-password","new_password":"new-self-service-secret"}`, memberCookie, "https://console.example", ""), 401)
	expect(call(selfPath, fmt.Sprintf(`{"current_password":%q,"new_password":"new-self-service-secret","identity_id":%q}`, password, adminID), memberCookie, "https://console.example", ""), 400)
	expect(call(selfPath, fmt.Sprintf(`{"current_password":%q,"new_password":"ocserviapassword"}`, password), memberCookie, "https://console.example", ""), 400)
	expect(call(selfPath, fmt.Sprintf(`{"current_password":%q,"new_password":"new-self-service-secret"}`, password), memberCookie, "https://sibling.example", ""), 403)
	expect(call(selfPath, fmt.Sprintf(`{"current_password":%q,"new_password":"new-self-service-secret"}`, password), memberCookie, "https://console.example", ""), 204)
	if _, err := svc.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("self change retained session: %v", err)
	}
	memberCookie, _, err = svc.AuthenticateLocal(ctx, "member-"+username, "new-self-service-secret")
	if err != nil {
		t.Fatal(err)
	}
	expect(call("local-users", body, memberCookie, "https://console.example", ""), 403)
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'UserManager','workspace',now())`, uuid.Must(uuid.NewV7()), id, workspaceID); err != nil {
		t.Fatal(err)
	}
	expect(call("local-users", body, memberCookie, "https://console.example", ""), 403)
	// A PlatformAdmin of another workspace cannot manage shared login accounts.
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Other',$2,now(),now())`, otherWorkspace, "other-"+username); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'PlatformAdmin','workspace',now())`, uuid.Must(uuid.NewV7()), id, otherWorkspace); err != nil {
		t.Fatal(err)
	}
	expect(call("local-users/"+id.String()+":disable", `{}`, memberCookie, "https://console.example", ""), 403)
	resetPath := "local-users/" + id.String() + ":reset-password"
	expect(call(resetPath, `{"password":"new-p4-password"}`, adminCookie, "https://console.example", ""), 409)
	// The independent approver comes only from the production one-shot path.
	approverCookie, _, err := svc.AuthenticateLocal(ctx, approverName, approverPassword)
	if err != nil {
		t.Fatal(err)
	}
	expect(call("local-users", body, approverCookie, "https://console.example", ""), 403)
	w = call("approval-requests", fmt.Sprintf(`{"action":"local_user.reset-password","resource_type":"local_user","resource_id":%q,"reason":"rotate credential","ttl_seconds":600}`, id), adminCookie, "https://console.example", "")
	expect(w, 201)
	var approval approvals.Approval
	if err := json.Unmarshal(w.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	decision := fmt.Sprintf(`{"reason":"verified request","expected_request_hash":%q}`, approval.RequestHash)
	expect(call("approval-requests/"+approval.ID.String()+":approve", decision, adminCookie, "https://console.example", ""), 403)
	expect(call("approval-requests/"+approval.ID.String()+":approve", decision, approverCookie, "https://console.example", ""), 200)
	var before, after string
	state := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, `SELECT json_build_array((SELECT row_to_json(c) FROM local_credentials c WHERE identity_id=$1),(SELECT json_agg(s ORDER BY id) FROM auth_sessions s WHERE identity_id=$1),(SELECT row_to_json(a) FROM approval_requests a WHERE id=$2),(SELECT count(*) FROM audit_events WHERE workspace_id=$3),(SELECT row_to_json(i) FROM identities i WHERE id=$1),(SELECT count(*) FROM identities),(SELECT count(*) FROM local_credentials),(SELECT count(*) FROM role_bindings),(SELECT count(*) FROM auth_sessions))::text`, id, approval.ID, workspaceID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, invalidBytes := range []string{"\xff", "\xfe", "\xc0\xaf", "\xed\xa0\x80"} {
		// Do not JSON-marshal: that would replace the invalid bytes in the fixture.
		invalidPassword := "otherwise-valid-password" + invalidBytes
		for _, request := range []struct{ path, body string }{
			{"local-users", fmt.Sprintf(`{"username":%q,"password":"`, "invalid-"+username) + invalidPassword + `"}`},
			{resetPath, `{"password":"` + invalidPassword + `"}`},
		} {
			before = state()
			response := call(request.path, request.body, adminCookie, "https://console.example", approval.ID.String())
			expect(response, 400)
			if strings.Contains(response.Body.String(), "otherwise-valid-password") {
				t.Fatal("invalid UTF-8 response disclosed password")
			}
			if after = state(); before != after {
				t.Fatal("invalid UTF-8 changed identity, credential, role, approval, session or audit state")
			}
		}
	}
	before = state()
	expect(call(resetPath, fmt.Sprintf(`{"password":%q}`, weakPassword), adminCookie, "https://console.example", approval.ID.String()), 400)
	after = state()
	if before != after {
		t.Fatal("policy rejection changed credential, sessions, approval or audit")
	}
	if _, err := svc.Authenticate(ctx, memberCookie); err != nil {
		t.Fatalf("policy rejection revoked session: %v", err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT p4_reject_reset CHECK (action <> 'local_user.reset-password') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	seedAttempt := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO local_auth_attempts(username,failures,window_until,blocked_until,lease_id,lease_until,expires_at) VALUES($1,5,now()+interval '15 minutes',now()+interval '1 minute',$2,now()+interval '30 seconds',now()+interval '15 minutes')`, "member-"+username, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	attemptCount := func(want int) {
		t.Helper()
		var got int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM local_auth_attempts WHERE username=$1`, "member-"+username).Scan(&got); err != nil || got != want {
			t.Fatalf("lifecycle attempt state: %d want %d err=%v", got, want, err)
		}
	}
	seedAttempt()
	expect(call(resetPath, `{"password":"new-p4-password"}`, adminCookie, "https://console.example", approval.ID.String()), 500)
	attemptCount(1)
	if _, err := svc.Authenticate(ctx, memberCookie); err != nil {
		t.Fatalf("failed reset revoked session: %v", err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT p4_reject_reset`); err != nil {
		t.Fatal(err)
	}
	expect(call(resetPath, `{"password":"new-p4-password"}`, adminCookie, "https://console.example", approval.ID.String()), 204)
	attemptCount(0)
	expect(call(resetPath, `{"password":"replay-password"}`, adminCookie, "https://console.example", approval.ID.String()), 409)
	if _, err := svc.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("reset session not revoked: %v", err)
	}
	if _, _, err := svc.AuthenticateLocal(ctx, "member-"+username, password); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old password accepted: %v", err)
	}
	memberCookie, _, err = svc.AuthenticateLocal(ctx, "member-"+username, "new-p4-password")
	if err != nil {
		t.Fatal(err)
	}
	seedAttempt()
	expect(call("local-users/"+id.String()+":disable", `{}`, adminCookie, "https://console.example", ""), 204)
	attemptCount(0)
	if _, err := svc.Authenticate(ctx, memberCookie); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled session accepted: %v", err)
	}
	if _, _, err := svc.AuthenticateLocal(ctx, "member-"+username, "new-p4-password"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("disabled login accepted: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE identity_id=$1 AND revoked_at IS NULL`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unrevoked sessions: %d %v", count, err)
	}
	oidcID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO identities(id,issuer,subject,email,created_at,updated_at) VALUES($1,'https://idp.example',$2,'same@example.test',now(),now())`, oidcID, "member-"+username); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"disable", "reset-password"} {
		requestBody := `{}`
		if action == "reset-password" {
			requestBody = `{"password":"unchanged-oidc-password"}`
		}
		expect(call("local-users/"+oidcID.String()+":"+action, requestBody, adminCookie, "https://console.example", ""), 404)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE id=$1 AND disabled_at IS NULL AND email='same@example.test'`, oidcID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("OIDC modified: %d %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND resource_id=$2 AND action IN ('local_user.create','local_user.disable','local_user.reset-password') AND actor_id=$3 AND source_session_id IS NOT NULL AND result='succeeded'`, workspaceID, id, adminID.String()).Scan(&count); err != nil || count != 3 {
		t.Fatalf("mutation audits: %d %v", count, err)
	}
	var approverID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT approver_identity_id FROM local_auth_bootstrap WHERE singleton`).Scan(&approverID); err != nil {
		t.Fatal(err)
	}
	expect(call("local-users/"+approverID.String()+":disable", `{}`, adminCookie, "https://console.example", ""), 409)
	var auditJSON string
	if err := pool.QueryRow(ctx, `SELECT json_agg(a)::text FROM audit_events a WHERE workspace_id=$1`, workspaceID).Scan(&auditJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String()+auditJSON, password) || strings.Contains(logs.String()+auditJSON, "new-p4-password") || strings.Contains(logs.String()+auditJSON, weakPassword) || strings.Contains(logs.String()+auditJSON, "$argon2id$") {
		t.Fatal("password leaked into logs/audit")
	}
	expect(call("local-users/"+adminID.String()+":disable", `{}`, adminCookie, "https://console.example", ""), 409)
	if _, err := bootstrap("replacement-"+username, password); !errors.Is(err, auth.ErrLocalInitialized) {
		t.Fatalf("protected admin reopened bootstrap: %v", err)
	}
}
