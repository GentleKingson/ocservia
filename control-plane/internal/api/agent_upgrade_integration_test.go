package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
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
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/testfixture"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAgentRolloutFleetLifecycleIntegration proves the durable canary and
// batch advancement flow: server-side eligibility and exclusions, stable
// ordering with exactly one canary, single-admin workspace authorization,
// idempotent creation, canary-first dispatch, no duplicate dispatch on repeated
// passes, failure and unknown outcomes pausing the rollout, resume requeueing
// only the non-succeeded nodes with a fresh upgrade attempt, and terminal
// completion once every batch has succeeded.
func TestAgentRolloutFleetLifecycleIntegration(t *testing.T) {
	databaseURL := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	testutil.NoError(t, err)
	defer pool.Close()
	ownerPool := pool
	if ownerURL := os.Getenv("OCSERV_TEST_OWNER_DATABASE_URL"); ownerURL != "" && ownerURL != databaseURL {
		ownerPool, err = pgxpool.New(ctx, ownerURL)
		testutil.NoError(t, err)
		defer ownerPool.Close()
	}
	workspaceID, operatorID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	operatorSession := uuid.Must(uuid.NewV7())
	otherWorkspaceID, otherNodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	nodeIDs := []uuid.UUID{uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())}
	slices.SortFunc(nodeIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	canaryID, batchID, otherID := nodeIDs[0], nodeIDs[1], nodeIDs[2]
	offlineNodeID, incapableNodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	unboundUserID, foreignViewerID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	allNodes := append([]uuid.UUID{offlineNodeID, incapableNodeID}, nodeIDs...)
	fixtures := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'rollout api',$2,now(),now()),($3,'rollout other',$4,now(),now())", []any{workspaceID, "rollout-api-" + workspaceID.String(), otherWorkspaceID, "rollout-other-" + otherWorkspaceID.String()}},
		{"INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'integration',$2,now(),now()),($3,'integration',$4,now(),now()),($5,'integration',$6,now(),now())", []any{operatorID, operatorID.String(), unboundUserID, unboundUserID.String(), foreignViewerID, foreignViewerID.String()}},
		{"INSERT INTO auth_sessions(id,identity_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())", []any{operatorSession, operatorID}},
		{"INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,now()),($3,$4,now())", []any{workspaceID, operatorID, otherWorkspaceID, foreignViewerID}},
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
		}{"UPDATE node_observed_snapshots SET agent_version='2.0.0' WHERE node_id=$1", []any{canaryID}},
		struct {
			query string
			args  []any
		}{"UPDATE node_capabilities SET capability='ocserv.agent.upgrade.v1' WHERE node_id=$1", []any{canaryID}},
		struct {
			query string
			args  []any
		}{"INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.agent.upgrade.v2',false)", []any{canaryID}},
		struct {
			query string
			args  []any
		}{"UPDATE node_capabilities SET approved=false WHERE node_id=$1", []any{incapableNodeID}},
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
	for _, nodeID := range []uuid.UUID{canaryID, batchID, otherID} {
		if _, err := testfixture.InstallKey(ctx, pool, nodeID); err != nil {
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
			{"DELETE FROM workspace_admins WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM auth_sessions WHERE id=$1", []any{operatorSession}},
			{"DELETE FROM identities WHERE id IN ($1,$2,$3)", []any{operatorID, unboundUserID, foreignViewerID}},
			{"DELETE FROM node_observed_snapshots WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM node_capabilities WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM node_privd_attestation_keys WHERE node_id=ANY($3)", []any{nil, nil, allNodes}},
			{"DELETE FROM nodes WHERE workspace_id IN ($1,$2)", []any{workspace, workspaceOther}},
			{"DELETE FROM workspaces WHERE id IN ($1,$2)", []any{workspace, workspaceOther}},
		} {
			_, _ = ownerPool.Exec(context.Background(), statement.query, statement.args...)
		}
	}()

	trustedDigest := bytes.Repeat([]byte{0x63}, 32)
	manifest := filepath.Join(t.TempDir(), "agent-releases.json")
	manifestJSON := `{"releases":[{"version":"2.0.0","architecture":"amd64","package_sha256":"` + hex.EncodeToString(trustedDigest) + `"}]}`
	if err := os.WriteFile(manifest, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := releasecatalog.Load(manifest)
	testutil.NoError(t, err)
	authn, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	testutil.NoError(t, err)
	server := &Server{backend: postgres.WrapPool(pool), auth: authn, operations: apiOperationService(pool), releaseCatalog: catalog, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	rolloutBody := func(batchSize int, ids ...uuid.UUID) string {
		raw := make([]string, 0, len(ids))
		for _, id := range ids {
			raw = append(raw, `"`+id.String()+`"`)
		}
		return `{"target_version":"2.0.0","node_ids":[` + strings.Join(raw, ",") + `],"batch_size":` + strconv.Itoa(batchSize) + `,"reason":"fleet rollout integration"}`
	}

	requested := append([]uuid.UUID{}, allNodes...)
	slices.SortFunc(requested, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	if foreign := postRollout("rollout-foreign", rolloutBody(2, otherNodeID)); foreign.Code != http.StatusBadRequest {
		t.Fatalf("foreign workspace node status=%d body=%s", foreign.Code, foreign.Body.String())
	}
	if unbounded := postRollout("rollout-unbounded", rolloutBody(25, nodeIDs...)); unbounded.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch status=%d body=%s", unbounded.Code, unbounded.Body.String())
	}

	createBody := rolloutBody(2, requested[4], requested[0], requested[2], requested[3], requested[1])
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
	rolloutID := uuid.MustParse(created.ID)
	if created.State != "queued" || created.CurrentBatch != 0 {
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
	assertExclusions := func(label string, rollout rolloutResponse) {
		t.Helper()
		reasons := map[string]string{}
		for _, exclusion := range rollout.Excluded {
			reasons[exclusion.NodeID] = exclusion.Reason
		}
		if len(reasons) != 2 || reasons[offlineNodeID.String()] != "offline" || reasons[incapableNodeID.String()] != "missing_capability" {
			t.Fatalf("%s rollout exclusions = %+v", label, rollout.Excluded)
		}
	}
	assertExclusions("created", created)
	replay := postRollout("rollout-create-1", createBody)
	if replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replayed rollout status=%d replayed=%q", replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	var replayed rolloutResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	assertExclusions("replayed", replayed)
	if replayed.ID != rolloutID.String() || replay.Header().Get("Location") != "/api/v1/agent-rollouts/"+rolloutID.String() {
		t.Fatalf("replay returned a different rollout: %+v location=%q", replayed, replay.Header().Get("Location"))
	}
	got := getRollout(rolloutID)
	if got.Code != http.StatusOK {
		t.Fatalf("get rollout status=%d body=%s", got.Code, got.Body.String())
	}
	listRollouts := func(actor auth.Principal, workspaceHeader string) (*httptest.ResponseRecorder, error) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/agent-rollouts", nil)
		if workspaceHeader != "" {
			request.Header.Set("X-Workspace-ID", workspaceHeader)
		}
		authorized, err := server.authorizeRoute(request, actor)
		if err != nil {
			return nil, err
		}
		response := httptest.NewRecorder()
		server.listAgentRollouts(response, request.WithContext(authorized))
		return response, nil
	}
	if listed, err := listRollouts(operator, ""); err != nil || listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), rolloutID.String()) {
		t.Fatalf("workspace admin rollout list err=%v response=%+v", err, listed)
	}
	unboundUser := auth.Principal{IdentityID: unboundUserID, Subject: unboundUserID.String(), Issuer: "integration"}
	if listed, err := listRollouts(unboundUser, ""); err == nil {
		t.Fatalf("unbound identity listed workspace rollouts status=%d body=%s", listed.Code, listed.Body.String())
	}
	foreignViewer := auth.Principal{IdentityID: foreignViewerID, Subject: foreignViewerID.String(), Issuer: "integration"}
	if listed, err := listRollouts(foreignViewer, workspaceID.String()); err == nil {
		t.Fatalf("foreign workspace admin listed rollouts status=%d body=%s", listed.Code, listed.Body.String())
	}
	var fetched rolloutResponse
	if err := json.Unmarshal(got.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	assertExclusions("fetched", fetched)
	if conflict := postRollout("rollout-create-1", rolloutBody(5, nodeIDs...)); conflict.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	replayRequest := func(actor auth.Principal) *http.Request {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-rollouts", strings.NewReader(createBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "rollout-create-1")
		authorized, err := server.authorizeRoute(request, actor)
		if err != nil {
			t.Fatalf("authorize rollout replay: %v", err)
		}
		return request.WithContext(authorized)
	}
	revokedReplay := replayRequest(operator)
	if _, err := ownerPool.Exec(ctx, "DELETE FROM workspace_admins WHERE workspace_id=$1 AND identity_id=$2", workspaceID, operatorID); err != nil {
		t.Fatal(err)
	}
	revokedResp := httptest.NewRecorder()
	server.createAgentRollout(revokedResp, revokedReplay)
	if revokedResp.Code != http.StatusForbidden || revokedResp.Header().Get("Location") != "" || revokedResp.Header().Get("Idempotency-Replayed") != "" || strings.Contains(revokedResp.Body.String(), rolloutID.String()) {
		t.Fatalf("revoked admin replay status=%d headers=%v body=%s", revokedResp.Code, revokedResp.Header(), revokedResp.Body.String())
	}
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_admins(workspace_id,identity_id,created_at) VALUES($1,$2,now())", workspaceID, operatorID); err != nil {
		t.Fatal(err)
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

	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(canaryID) != 1 || countNodeUpgrades(batchID) != 0 || countNodeUpgrades(otherID) != 0 {
		t.Fatalf("canary dispatch must be exactly one node upgrade")
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(canaryID) != 1 {
		t.Fatalf("repeated advance duplicated the canary upgrade")
	}

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

	setState(canaryID, "succeeded")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countNodeUpgrades(batchID) != 1 || countNodeUpgrades(otherID) != 1 {
		t.Fatalf("batch one must dispatch after canary success")
	}

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

	createRollout := func(key string, batchSize int) uuid.UUID {
		t.Helper()
		response := postRollout(key, rolloutBody(batchSize, nodeIDs...))
		if response.Code != http.StatusCreated {
			t.Fatalf("create rollout %s status=%d body=%s", key, response.Code, response.Body.String())
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return uuid.MustParse(body.ID)
	}
	countRolloutNodeUpgrades := func(rID, nID uuid.UUID) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_upgrade_operations u JOIN operations o ON o.id=u.operation_id WHERE o.request_id=$1", "agent-rollout:"+rID.String()+":"+nID.String()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	completeRolloutUpgrades := func(rID uuid.UUID, filter string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE agent_upgrade_operations u SET state='succeeded',completed_at=now() FROM operations o WHERE o.id=u.operation_id AND o.request_id LIKE $1 AND u.completed_at IS NULL", filter); err != nil {
			t.Fatal(err)
		}
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

	canarySkipRolloutID := createRollout("rollout-canary-skip", 2)
	if _, err := pool.Exec(ctx, "UPDATE nodes SET status='offline' WHERE id=$1", canaryID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	assertPausedSkip(canarySkipRolloutID, canaryID, 0, "offline")
	if countRolloutNodeUpgrades(canarySkipRolloutID, canaryID)+countRolloutNodeUpgrades(canarySkipRolloutID, batchID)+countRolloutNodeUpgrades(canarySkipRolloutID, otherID) != 0 {
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
	if countRolloutNodeUpgrades(canarySkipRolloutID, canaryID) != 1 || countRolloutNodeUpgrades(canarySkipRolloutID, batchID) != 0 || countRolloutNodeUpgrades(canarySkipRolloutID, otherID) != 0 {
		t.Fatal("resuming a skipped canary must retry the canary before ordinary batches")
	}
	completeRolloutUpgrades(canarySkipRolloutID, "agent-rollout:"+canarySkipRolloutID.String()+":%")
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countRolloutNodeUpgrades(canarySkipRolloutID, batchID) != 1 || countRolloutNodeUpgrades(canarySkipRolloutID, otherID) != 1 {
		t.Fatal("ordinary batches may start only after the retried canary succeeds")
	}
	completeRolloutUpgrades(canarySkipRolloutID, "agent-rollout:"+canarySkipRolloutID.String()+":%")
	if err := advance(); err != nil {
		t.Fatal(err)
	}

	batchSkipRolloutID := createRollout("rollout-batch-skip", 1)
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	if countRolloutNodeUpgrades(batchSkipRolloutID, canaryID) != 1 {
		t.Fatal("batch-skip rollout must dispatch its canary first")
	}
	completeRolloutUpgrades(batchSkipRolloutID, "agent-rollout:"+batchSkipRolloutID.String()+":"+canaryID.String())
	if _, err := pool.Exec(ctx, "UPDATE nodes SET status='offline' WHERE id=$1", batchID); err != nil {
		t.Fatal(err)
	}
	if err := advance(); err != nil {
		t.Fatal(err)
	}
	assertPausedSkip(batchSkipRolloutID, batchID, 1, "offline")
	if countRolloutNodeUpgrades(batchSkipRolloutID, batchID) != 0 || countRolloutNodeUpgrades(batchSkipRolloutID, otherID) != 0 {
		t.Fatal("an ineligible ordinary-batch node must hold it and every later batch")
	}
}
