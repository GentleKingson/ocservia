package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	approvalstore "github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/attestationtest"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAgentUpgradeRouteResolvesTrustedReleasesIntegration pins the API trust
// boundary: only the server-side release catalog may turn a target version
// into a package digest, the target must be a real upgrade, the approval must
// match the resolved release identity, and only operators may call it.
func TestAgentUpgradeRouteResolvesTrustedReleasesIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	workspaceID, operatorID, viewerID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	operatorSession, viewerSession, approverID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeID, operatorBinding, viewerBinding := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	fixtures := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'upgrade api',$2,now(),now())`, []any{workspaceID, "upgrade-api-" + workspaceID.String()}},
		{`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'integration',$2,now(),now()),($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now())`, []any{operatorID, operatorID.String(), viewerID, viewerID.String(), approverID, approverID.String()}},
		{`INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now()),($3,$4,now()+interval '1 hour',now())`, []any{operatorSession, operatorID, viewerSession, viewerID}},
		{`INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'upgrade-api-node','active',1,now(),now())`, []any{nodeID, workspaceID}},
		{`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v1',true)`, []any{nodeID}}, {`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at)
			VALUES($1,now(),'upgrade-api-boot',$2,'1.2.0','1.2.3','Debian GNU/Linux 12','amd64','{}','{}','{}',now())`, []any{nodeID, uuid.Must(uuid.NewV7())}},
		{`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($1,$2,$3,'Operator','node',$4,$2,now()),($5,$6,$3,'Viewer','node',$4,$6,now())`, []any{operatorBinding, operatorID, workspaceID, nodeID, viewerBinding, viewerID}},
	}
	for _, fixture := range fixtures {
		if _, err := pool.Exec(ctx, fixture.query, fixture.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Privileged synthetic operations additionally require an active privd
	// attestation key, including the agent upgrade acknowledgement path.
	if _, err := attestationtest.InstallKey(ctx, pool, nodeID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, statement := range []string{
			`DELETE FROM node_agent_upgrade_results WHERE node_id=$2`,
			`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`,
			`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`,
			`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`,
			`DELETE FROM commands WHERE workspace_id=$1`, `DELETE FROM operations WHERE workspace_id=$1`,
			`DELETE FROM approval_requests WHERE workspace_id=$1`, `DELETE FROM audit_events WHERE workspace_id=$1`,
			`DELETE FROM role_bindings WHERE id IN($3,$4)`, `DELETE FROM auth_sessions WHERE id IN($5,$6)`,
			`DELETE FROM identities WHERE id IN($7,$8,$9)`, `DELETE FROM node_observed_snapshots WHERE node_id=$2`,
			`DELETE FROM node_capabilities WHERE node_id=$2`, `DELETE FROM nodes WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			_, _ = pool.Exec(context.Background(), statement, workspaceID, nodeID, operatorBinding, viewerBinding, operatorSession, viewerSession, operatorID, viewerID, approverID)
		}
	}()

	trustedDigest := bytes.Repeat([]byte{0x43}, 32)
	olderDigest := bytes.Repeat([]byte{0x44}, 32)
	manifest := filepath.Join(t.TempDir(), "agent-releases.json")
	manifestJSON := `{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"` + hex.EncodeToString(trustedDigest) + `"},{"version":"1.0.0","architecture":"amd64","package_sha256":"` + hex.EncodeToString(olderDigest) + `"}]}`
	if err := os.WriteFile(manifest, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{pool: pool, rbac: rbac.New(pool), operations: apiOperationService(pool)}
	server.EnableReleaseCatalog(catalog)

	post := func(t *testing.T, principal auth.Principal, key, ifMatch, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/agent-upgrade", strings.NewReader(body))
		request.SetPathValue("node_id", nodeID.String())
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		if ifMatch != "" {
			request.Header.Set("If-Match", ifMatch)
		}
		request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		authorized, err := server.authorizeRoute(request, principal)
		if err != nil {
			t.Fatalf("authorize agent upgrade: %v", err)
		}
		request = request.WithContext(context.WithValue(authorized, requestIDKey{}, "request-"+key))
		response := httptest.NewRecorder()
		server.upgradeAgent(response, request)
		return response
	}
	operator := auth.Principal{IdentityID: operatorID, SessionID: operatorSession, Subject: operatorID.String(), Issuer: "integration"}
	viewer := auth.Principal{IdentityID: viewerID, SessionID: viewerSession, Subject: viewerID.String(), Issuer: "integration"}
	validBody := func(target string, approval string) string {
		return `{"target_version":"` + target + `","approval_id":"` + approval + `","reason":"roll out the maintained release"}`
	}

	// A viewer must never reach the handler.
	viewerRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+nodeID.String()+"/agent-upgrade", strings.NewReader(validBody("2.0.0", uuid.Must(uuid.NewV7()).String())))
	viewerRequest.SetPathValue("node_id", nodeID.String())
	viewerRequest.Header.Set("Content-Type", "application/json")
	if _, err := server.authorizeRoute(viewerRequest, viewer); err == nil {
		t.Fatal("viewer must not be authorized to upgrade an agent")
	}

	if response := post(t, operator, "upgrade-stale", `"revision-2"`, validBody("2.0.0", uuid.Must(uuid.NewV7()).String())); response.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(t, operator, "upgrade-untrusted", `"revision-1"`, validBody("3.0.0", uuid.Must(uuid.NewV7()).String())); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "release-not-trusted") {
		t.Fatalf("untrusted release status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(t, operator, "upgrade-older", `"revision-1"`, validBody("1.0.0", uuid.Must(uuid.NewV7()).String())); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "target-not-newer") {
		t.Fatalf("older release status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(t, operator, "upgrade-unapproved", `"revision-1"`, validBody("2.0.0", uuid.Must(uuid.NewV7()).String())); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "approval-required") {
		t.Fatalf("unapproved release status=%d body=%s", response.Code, response.Body.String())
	}

	// The approval is created through the approval surface bound to exactly
	// the server-resolved release identity.
	approvalID := uuid.Must(uuid.NewV7())
	approvalHash, approvalSummary := approvalstore.AgentUpgradeBinding(nodeID, "2.0.0", trustedDigest, "amd64")
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.upgrade','node',$4,'api integration','approved',$5,'independent approval',now()+interval '1 hour',now(),now(),$6,$7)`, approvalID, workspaceID, operatorID, nodeID, approverID, approvalHash, approvalSummary); err != nil {
		t.Fatal(err)
	}
	response := post(t, operator, "upgrade-accepted", `"revision-1"`, validBody("2.0.0", approvalID.String()))
	if response.Code != http.StatusAccepted {
		t.Fatalf("accepted upgrade status=%d body=%s", response.Code, response.Body.String())
	}
	var projectionState string
	var projectionDigest []byte
	if err := pool.QueryRow(ctx, `SELECT state,package_sha256 FROM agent_upgrade_operations WHERE node_id=$1`, nodeID).Scan(&projectionState, &projectionDigest); err != nil {
		t.Fatal(err)
	}
	if projectionState != "queued" || !bytes.Equal(projectionDigest, trustedDigest) {
		t.Fatalf("projection = %q/%x, want queued and the catalog-resolved digest", projectionState, projectionDigest)
	}
	if replay := post(t, operator, "upgrade-accepted", `"revision-1"`, validBody("2.0.0", approvalID.String())); replay.Code != http.StatusAccepted || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replayed upgrade status=%d replayed=%q", replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	// A second, independently approved request while the first is still
	// scheduled must fail on the single-active-upgrade rule.
	conflictApprovalID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.upgrade','node',$4,'api integration','approved',$5,'independent approval',now()+interval '1 hour',now(),now(),$6,$7)`, conflictApprovalID, workspaceID, operatorID, nodeID, approverID, approvalHash, approvalSummary); err != nil {
		t.Fatal(err)
	}
	if conflict := post(t, operator, "upgrade-conflict", `"revision-1"`, validBody("2.0.0", conflictApprovalID.String())); conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "upgrade-already-active") {
		t.Fatalf("concurrent upgrade status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}
