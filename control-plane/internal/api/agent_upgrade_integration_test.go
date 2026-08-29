package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		{`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',true)`, []any{nodeID}}, {`INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at)
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
	// Per-statement arguments: pgx rejects statements bound with more
	// parameters than they reference, which would silently leak the
	// agent upgrade command history the scripted rollback checks.
	defer func() {
		workspace, node := workspaceID, nodeID
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM node_agent_upgrade_results WHERE node_id=$1`, []any{node}},
			{`DELETE FROM agent_upgrade_operations WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id=$1)`, []any{workspace}},
			{`DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id=$1)`, []any{workspace}},
			{`DELETE FROM commands WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM operations WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM approval_requests WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM role_bindings WHERE id IN($1,$2)`, []any{operatorBinding, viewerBinding}},
			{`DELETE FROM auth_sessions WHERE id IN($1,$2)`, []any{operatorSession, viewerSession}},
			{`DELETE FROM identities WHERE id IN($1,$2,$3)`, []any{operatorID, viewerID, approverID}},
			{`DELETE FROM node_observed_snapshots WHERE node_id=$1`, []any{node}},
			{`DELETE FROM node_capabilities WHERE node_id=$1`, []any{node}},
			{`DELETE FROM node_privd_attestation_keys WHERE node_id=$1`, []any{node}},
			{`DELETE FROM nodes WHERE workspace_id=$1`, []any{workspace}},
			{`DELETE FROM workspaces WHERE id=$1`, []any{workspace}},
		} {
			_, _ = pool.Exec(context.Background(), statement.query, statement.args...)
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

// TestAgentRolloutFleetLifecycleIntegration proves the durable canary and
// batch advancement flow: server-side eligibility and exclusions, stable
// ordering with exactly one canary, approval request binding, idempotent
// creation, canary-first dispatch, no duplicate dispatch on repeated passes,
// failure and unknown outcomes pausing the rollout, resume requeueing only
// the non-succeeded nodes with a fresh upgrade attempt, and terminal
// completion once every batch has succeeded.
func TestAgentRolloutFleetLifecycleIntegration(t *testing.T) {
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
	workspaceID, operatorID, approverID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	operatorSession := uuid.Must(uuid.NewV7())
	rolloutID := uuid.Must(uuid.NewV7())
	otherWorkspaceID := uuid.Must(uuid.NewV7())
	otherNodeID := uuid.Must(uuid.NewV7())
	nodeIDs := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	slices.SortFunc(nodeIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	canaryID, batchID, otherID := nodeIDs[0], nodeIDs[1], nodeIDs[2]
	offlineNodeID, currentNodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	operatorBinding := uuid.Must(uuid.NewV7())
	allNodes := append([]uuid.UUID{offlineNodeID, currentNodeID}, nodeIDs...)
	fixtures := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'rollout api',$2,now(),now()),($3,'rollout other',$4,now(),now())", []any{workspaceID, "rollout-api-" + workspaceID.String(), otherWorkspaceID, "rollout-other-" + otherWorkspaceID.String()}},
		{"INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'integration',$2,now(),now()),($3,'integration',$4,now(),now())", []any{operatorID, operatorID.String(), approverID, approverID.String()}},
		{"INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())", []any{operatorSession, operatorID}},
		{"INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at) VALUES($1,$2,$3,'Operator','workspace',NULL,$2,now())", []any{operatorBinding, operatorID, workspaceID}},
	}
	for index, nodeID := range allNodes {
		fixtures = append(fixtures,
			struct {
				query string
				args  []any
			}{"INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,$3,'active',1,now(),now())", []any{nodeID, workspaceID, "rollout-node-" + strconv.Itoa(index+1)}},
			struct {
				query string
				args  []any
			}{"INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',true)", []any{nodeID}},
			struct {
				query string
				args  []any
			}{"INSERT INTO node_observed_snapshots(node_id,observed_at,boot_id,agent_instance_id,agent_version,ocserv_version,os_release,architecture,ocserv,system,path,last_heartbeat_at) VALUES($1,now(),$2,$3,'1.2.0','1.2.3','Debian GNU/Linux 12','amd64','{}','{}','{}',now())", []any{nodeID, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}},
		)
	}
	fixtures = append(fixtures,
		struct {
			query string
			args  []any
		}{"UPDATE nodes SET status='offline' WHERE id=$1", []any{offlineNodeID}},
		struct {
			query string
			args  []any
		}{"UPDATE node_observed_snapshots SET agent_version='2.0.0' WHERE node_id=$1", []any{currentNodeID}},
		struct {
			query string
			args  []any
		}{"INSERT INTO nodes(id,workspace_id,name,status,version,created_at,updated_at) VALUES($1,$2,'foreign-node','active',1,now(),now())", []any{otherNodeID, otherWorkspaceID}},
	)
	for _, fixture := range fixtures {
		if _, err := pool.Exec(ctx, fixture.query, fixture.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Privileged upgrade operations additionally require an active privd
	// attestation key on every dispatched node.
	for _, nodeID := range []uuid.UUID{canaryID, batchID, otherID} {
		if _, err := attestationtest.InstallKey(ctx, pool, nodeID); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		workspace, workspaceOther := workspaceID, otherWorkspaceID
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"DELETE FROM node_agent_upgrade_results WHERE node_id=ANY($1)", []any{allNodes}},
			{"DELETE FROM agent_rollout_nodes WHERE rollout_id IN (SELECT id FROM agent_rollouts WHERE workspace_id=$1)", []any{workspace}},
			{"DELETE FROM agent_rollouts WHERE workspace_id=$1", []any{workspace}},
			{"DELETE FROM agent_upgrade_operations WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM outbox_events WHERE command_id IN(SELECT id FROM commands WHERE workspace_id IN ($1,$2))", []any{workspace, workspaceOther}},
			{"DELETE FROM operation_events WHERE operation_id IN(SELECT id FROM operations WHERE workspace_id IN ($1,$2))", []any{workspace, workspaceOther}},
			{"DELETE FROM commands WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM operations WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM approval_requests WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM role_bindings WHERE id=$1", []any{operatorBinding}},
			{"DELETE FROM auth_sessions WHERE id=$1", []any{operatorSession}},
			{"DELETE FROM identities WHERE id IN ($1,$2)", []any{operatorID, approverID}},
			{"DELETE FROM node_observed_snapshots WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM node_capabilities WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM node_privd_attestation_keys WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM nodes WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM workspaces WHERE id IN ($1,$2)", []any{workspace, workspaceOther}},
		} {
			_, _ = pool.Exec(context.Background(), statement.query, statement.args...)
		}
	}()

	trustedDigest := bytes.Repeat([]byte{0x63}, 32)
	manifest := filepath.Join(t.TempDir(), "agent-releases.json")
	manifestJSON := `{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"` + hex.EncodeToString(trustedDigest) + `"}]}`
	if err := os.WriteFile(manifest, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{pool: pool, rbac: rbac.New(pool), operations: apiOperationService(pool), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server.EnableReleaseCatalog(catalog)
	server.operations.EnableReleaseCatalog(catalog)

	operator := auth.Principal{IdentityID: operatorID, SessionID: operatorSession, Subject: operatorID.String(), Issuer: "integration"}
	postRollout := func(key, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-rollouts", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		authorized, err := server.authorizeRoute(request, operator)
		if err != nil {
			t.Fatalf("authorize rollout create: %v", err)
		}
		request = request.WithContext(authorized)
		response := httptest.NewRecorder()
		server.createAgentRollout(response, request)
		return response
	}
	getRollout := func(id uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/agent-rollouts/"+id.String(), nil)
		request.SetPathValue("rollout_id", id.String())
		authorized, err := server.authorizeRoute(request, operator)
		if err != nil {
			t.Fatalf("authorize rollout get: %v", err)
		}
		request = request.WithContext(authorized)
		response := httptest.NewRecorder()
		server.getAgentRollout(response, request)
		return response
	}
	rolloutBody := func(batchSize int, approval string, ids ...uuid.UUID) string {
		raw := make([]string, 0, len(ids))
		for _, id := range ids {
			raw = append(raw, `"`+id.String()+`"`)
		}
		return `{"target_version":"2.0.0","node_ids":[` + strings.Join(raw, ",") + `],"batch_size":` + strconv.Itoa(batchSize) + `,"reason":"fleet rollout integration","approval_id":"` + approval + `"}`
	}

	requested := append([]uuid.UUID{}, allNodes...)
	slices.SortFunc(requested, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	// The approval is bound to the canonical sorted request: target version,
	// sorted node set, batch size two, stop-on-failure.
	requestHash, requestSummary := approvalstore.AgentRolloutBinding("2.0.0", requested, 2, true)
	approvalID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, "INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.rollout','batch_operation',$4,'fleet rollout integration','approved',$5,'independent approval',now()+interval '1 hour',now(),now(),$6,$7)", approvalID, workspaceID, operatorID, rolloutID, approverID, requestHash, requestSummary); err != nil {
		t.Fatal(err)
	}

	// A foreign workspace node can never satisfy the approval binding: the
	// request hash covers the exact approved node set, so the mismatch fails
	// closed before any eligibility lookup.
	if foreign := postRollout("rollout-foreign", rolloutBody(2, approvalID.String(), otherNodeID)); foreign.Code != http.StatusConflict {
		t.Fatalf("foreign workspace node status=%d body=%s", foreign.Code, foreign.Body.String())
	}
	// Batch sizes are bounded.
	if unbounded := postRollout("rollout-unbounded", rolloutBody(25, approvalID.String(), nodeIDs...)); unbounded.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch status=%d body=%s", unbounded.Code, unbounded.Body.String())
	}
	// A mismatched approval binding fails closed: a different node set hashes
	// differently than the approved request.
	if mismatch := postRollout("rollout-mismatch", rolloutBody(2, approvalID.String(), nodeIDs...)); mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched approval status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}

	createBody := rolloutBody(2, approvalID.String(), requested[4], requested[0], requested[2], requested[3], requested[1])
	response := postRollout("rollout-create-1", createBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("created rollout status=%d body=%s", response.Code, response.Body.String())
	}
	type rolloutResponse struct {
		ID           string `json:"id"`
		State        string `json:"state"`
		CurrentBatch int    `json:"current_batch"`
		Nodes        []struct {
			NodeID  string `json:"node_id"`
			Ordinal int    `json:"ordinal"`
			Batch   int    `json:"batch"`
			State   string `json:"state"`
		} `json:"nodes"`
		Excluded []struct {
			NodeID string `json:"node_id"`
			Reason string `json:"reason"`
		} `json:"excluded"`
	}
	var created rolloutResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != rolloutID.String() || created.State != "queued" || created.CurrentBatch != 0 {
		t.Fatalf("rollout header = %+v", created)
	}
	if len(created.Nodes) != 3 || len(created.Excluded) != 2 {
		t.Fatalf("rollout selection = %+v exclusions %+v", created.Nodes, created.Excluded)
	}
	for index, node := range created.Nodes {
		if node.NodeID != nodeIDs[index].String() || node.Ordinal != index || node.State != "pending" {
			t.Fatalf("node %d out of stable order: %+v", index, node)
		}
		if index == 0 && node.Batch != 0 {
			t.Fatalf("canary must be batch 0: %+v", node)
		}
		if index > 0 && node.Batch != 1 {
			t.Fatalf("batch 1 must hold the remaining nodes: %+v", node)
		}
	}
	exclusionReasons := map[string]string{}
	for _, exclusion := range created.Excluded {
		exclusionReasons[exclusion.NodeID] = exclusion.Reason
	}
	if exclusionReasons[offlineNodeID.String()] != "offline" || exclusionReasons[currentNodeID.String()] != "already_current" {
		t.Fatalf("exclusion reasons = %+v", created.Excluded)
	}
	assertExclusions := func(label string, rollout rolloutResponse) {
		t.Helper()
		reasons := map[string]string{}
		for _, exclusion := range rollout.Excluded {
			reasons[exclusion.NodeID] = exclusion.Reason
		}
		if len(reasons) != 2 || reasons[offlineNodeID.String()] != "offline" || reasons[currentNodeID.String()] != "already_current" {
			t.Fatalf("%s rollout exclusions = %+v", label, rollout.Excluded)
		}
	}
	replay := postRollout("rollout-create-1", createBody)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replayed rollout status=%d replayed=%q", replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	var replayed rolloutResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	assertExclusions("replayed", replayed)
	got := getRollout(rolloutID)
	if got.Code != http.StatusOK {
		t.Fatalf("get rollout status=%d body=%s", got.Code, got.Body.String())
	}
	var fetched rolloutResponse
	if err := json.Unmarshal(got.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	assertExclusions("fetched", fetched)
	if conflict := postRollout("rollout-create-1", rolloutBody(5, approvalID.String(), nodeIDs...)); conflict.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	advance := func() error {
		return server.operations.AdvanceAgentRollouts(ctx)
	}
	countNodeUpgrades := func(nodeID uuid.UUID) int {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_upgrade_operations WHERE node_id=$1", nodeID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	setState := func(nodeID uuid.UUID, state string) {
		if _, err := pool.Exec(ctx, "UPDATE agent_upgrade_operations SET state=$2,completed_at=now() WHERE node_id=$1 AND completed_at IS NULL", nodeID, state); err != nil {
			t.Fatal(err)
		}
	}

	// First advance: the queued rollout starts and dispatches the canary only.
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(canaryID) != 1 || countNodeUpgrades(batchID) != 0 || countNodeUpgrades(otherID) != 0 {
		t.Fatalf("canary dispatch must be exactly one node upgrade")
	}
	// Repeated passes are idempotent: no duplicate upgrade operation.
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(canaryID) != 1 {
		t.Fatalf("repeated advance duplicated the canary upgrade")
	}

	// Unknown outcome pauses the rollout.
	setState(canaryID, "unknown")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	var rolloutState, pauseCode string
	if err := pool.QueryRow(ctx, "SELECT state,pause_code FROM agent_rollouts WHERE id=$1", rolloutID).Scan(&rolloutState, &pauseCode); err != nil {
		t.Fatal(err)
	}
	if rolloutState != "paused" || pauseCode != "node_unknown" {
		t.Fatalf("unknown outcome must pause: state=%s pause=%s", rolloutState, pauseCode)
	}

	// Resume requeues the unknown node with a fresh attempt; the next pass
	// dispatches attempt two instead of replaying the old operation.
	resume := func(id uuid.UUID, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-rollouts/"+id.String()+"/resume", strings.NewReader("{}"))
		request.SetPathValue("rollout_id", id.String())
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		authorized, err := server.authorizeRoute(request, operator)
		if err != nil {
			t.Fatalf("authorize rollout resume: %v", err)
		}
		request = request.WithContext(authorized)
		recorder := httptest.NewRecorder()
		server.resumeAgentRollout(recorder, request)
		return recorder
	}
	if resumed := resume(rolloutID, "rollout-resume-1"); resumed.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(canaryID) != 2 {
		t.Fatalf("resume must dispatch a fresh upgrade attempt, got %d operations", countNodeUpgrades(canaryID))
	}

	// Canary succeeds; batch one starts and both nodes are dispatched.
	setState(canaryID, "succeeded")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(batchID) != 1 || countNodeUpgrades(otherID) != 1 {
		t.Fatalf("batch one must dispatch after canary success")
	}

	// One failure pauses; succeeded nodes are never redispatched on resume.
	setState(batchID, "failed")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT state,pause_code FROM agent_rollouts WHERE id=$1", rolloutID).Scan(&rolloutState, &pauseCode); err != nil {
		t.Fatal(err)
	}
	if rolloutState != "paused" || pauseCode != "node_failed" {
		t.Fatalf("failure must pause: state=%s pause=%s", rolloutState, pauseCode)
	}
	if resumed := resume(rolloutID, "rollout-resume-2"); resumed.Code != http.StatusOK {
		t.Fatalf("second resume status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(batchID) != 2 || countNodeUpgrades(otherID) != 1 {
		t.Fatalf("resume must requeue only the failed node: batch=%d other=%d", countNodeUpgrades(batchID), countNodeUpgrades(otherID))
	}

	// The final success completes the rollout.
	setState(batchID, "succeeded")
	setState(otherID, "succeeded")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT state FROM agent_rollouts WHERE id=$1", rolloutID).Scan(&rolloutState); err != nil {
		t.Fatal(err)
	}
	if rolloutState != "succeeded" {
		t.Fatalf("completed rollout state=%s, want succeeded", rolloutState)
	}

	createApprovedRollout := func(id uuid.UUID, key string, batchSize int) uuid.UUID {
		t.Helper()
		hash, summary := approvalstore.AgentRolloutBinding("2.0.0", nodeIDs, batchSize, true)
		approvalID := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, "INSERT INTO approval_requests(id,workspace_id,requester_id,action,resource_type,resource_id,reason,status,approver_id,approval_reason,expires_at,approved_at,created_at,request_hash,request_summary) VALUES($1,$2,$3,'agent.rollout','batch_operation',$4,'fleet rollout integration','approved',$5,'independent approval',now()+interval '1 hour',now(),now(),$6,$7)", approvalID, workspaceID, operatorID, id, approverID, hash, summary); err != nil {
			t.Fatal(err)
		}
		response := postRollout(key, rolloutBody(batchSize, approvalID.String(), nodeIDs...))
		if response.Code != http.StatusCreated {
			t.Fatalf("create rollout %s status=%d body=%s", id, response.Code, response.Body.String())
		}
		return approvalID
	}
	countRolloutNodeUpgrades := func(approvalID, nodeID uuid.UUID) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_upgrade_operations WHERE approval_id=$1 AND node_id=$2", approvalID, nodeID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	assertPausedSkip := func(id, nodeID uuid.UUID, wantBatch int, reason string) {
		t.Helper()
		var state, code, nodeState, failureCode string
		var currentBatch int
		if err := pool.QueryRow(ctx, "SELECT state,pause_code,current_batch FROM agent_rollouts WHERE id=$1", id).Scan(&state, &code, &currentBatch); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT state,failure_code FROM agent_rollout_nodes WHERE rollout_id=$1 AND node_id=$2", id, nodeID).Scan(&nodeState, &failureCode); err != nil {
			t.Fatal(err)
		}
		if state != "paused" || code != "node_skipped" || currentBatch != wantBatch || nodeState != "skipped" || failureCode != reason {
			t.Fatalf("skipped rollout %s = state %s/%s batch %d node %s/%s", id, state, code, currentBatch, nodeState, failureCode)
		}
	}

	// A canary that becomes ineligible after creation pauses batch zero. No
	// later batch operation may be created without an operator resume.
	canarySkipRolloutID := uuid.Must(uuid.NewV7())
	canarySkipApprovalID := createApprovedRollout(canarySkipRolloutID, "rollout-canary-skip", 2)
	if _, err := pool.Exec(ctx, "UPDATE nodes SET status='offline' WHERE id=$1", canaryID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	assertPausedSkip(canarySkipRolloutID, canaryID, 0, "offline")
	if countRolloutNodeUpgrades(canarySkipApprovalID, canaryID)+countRolloutNodeUpgrades(canarySkipApprovalID, batchID)+countRolloutNodeUpgrades(canarySkipApprovalID, otherID) != 0 {
		t.Fatal("an ineligible canary must not dispatch any rollout operation")
	}
	if _, err := pool.Exec(ctx, "UPDATE nodes SET status='active' WHERE id=$1", canaryID); err != nil {
		t.Fatal(err)
	}
	if resumed := resume(canarySkipRolloutID, "rollout-resume-skipped-canary"); resumed.Code != http.StatusOK {
		t.Fatalf("resume skipped canary status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countRolloutNodeUpgrades(canarySkipApprovalID, canaryID) != 1 || countRolloutNodeUpgrades(canarySkipApprovalID, batchID) != 0 || countRolloutNodeUpgrades(canarySkipApprovalID, otherID) != 0 {
		t.Fatal("resuming a skipped canary must retry the canary before ordinary batches")
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_upgrade_operations SET state='succeeded',completed_at=now() WHERE approval_id=$1 AND completed_at IS NULL", canarySkipApprovalID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countRolloutNodeUpgrades(canarySkipApprovalID, batchID) != 1 || countRolloutNodeUpgrades(canarySkipApprovalID, otherID) != 1 {
		t.Fatal("ordinary batches may start only after the retried canary succeeds")
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_upgrade_operations SET state='succeeded',completed_at=now() WHERE approval_id=$1 AND completed_at IS NULL", canarySkipApprovalID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}

	// If an ordinary-batch node loses eligibility while the canary runs, its
	// batch remains current and the following batch is not dispatched.
	batchSkipRolloutID := uuid.Must(uuid.NewV7())
	batchSkipApprovalID := createApprovedRollout(batchSkipRolloutID, "rollout-batch-skip", 1)
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countRolloutNodeUpgrades(batchSkipApprovalID, canaryID) != 1 {
		t.Fatal("batch-skip rollout must dispatch its canary first")
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_upgrade_operations SET state='succeeded',completed_at=now() WHERE approval_id=$1 AND node_id=$2 AND completed_at IS NULL", batchSkipApprovalID, canaryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE nodes SET status='offline' WHERE id=$1", batchID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	assertPausedSkip(batchSkipRolloutID, batchID, 1, "offline")
	if countRolloutNodeUpgrades(batchSkipApprovalID, batchID) != 0 || countRolloutNodeUpgrades(batchSkipApprovalID, otherID) != 0 {
		t.Fatal("an ineligible ordinary-batch node must hold it and every later batch")
	}
}
