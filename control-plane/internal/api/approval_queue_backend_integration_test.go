package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestPendingApprovalQueueBackendIntegration(t *testing.T) {
	f := newApplyHTTPFixture(t)
	var indexed bool
	if err := f.row(`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='public' AND tablename='approval_requests' AND indexname='approval_requests_scope_idx' AND indexdef LIKE '%(workspace_id, resource_type,%')`, `SELECT EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='approval_requests' AND index_name='approval_requests_scope_idx' AND seq_in_index=1 AND column_name='workspace_id')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("workspace prefix index unavailable: %v %v", indexed, err)
	}

	at, _ := value.FromTime(time.Now().UTC())
	nodeA, nodeB, foreignWorkspace, foreignNode := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Other approvals',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Other approvals',?,?,?)`, foreignWorkspace, foreignWorkspace.String(), at, at)
	for _, pair := range [][2]uuid.UUID{{nodeA, f.workspace}, {nodeB, f.workspace}, {foreignNode, foreignWorkspace}} {
		f.exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,$3,'active',$4,$5)`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,?,'active',?,?)`, pair[0], pair[1], pair[0].String(), at, at)
	}
	f.bind(f.approver, nodeA, "SecurityAdmin")
	f.bind(f.requester, nodeA, "SecurityAdmin")
	f.bind(f.requester, nodeB, "SecurityAdmin")
	// A separate workspace is also authorized, so filtering cannot depend merely on role presence.
	f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES($1,$2,$3,'SecurityAdmin','node',$4,$5)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES(?,?,?,'SecurityAdmin','node',?,?)`, uuid.Must(uuid.NewV7()), f.approver.principal.IdentityID, foreignWorkspace, foreignNode, value.Timestamp{Valid: true})
	create := func(actor applyHTTPActor, workspace, node uuid.UUID, scopes ...uuid.UUID) approvals.Approval {
		t.Helper()
		hash, summary := approvals.GenericBinding("service.reload", "node", node)
		authority := []approvals.AuthorityResource{}
		for _, id := range scopes {
			authority = append(authority, approvals.AuthorityResource{WorkspaceID: workspace, Type: "node", ID: id})
		}
		a, err := f.s.approvals.Create(t.Context(), approvals.Request{WorkspaceID: workspace, RequesterID: actor.principal.IdentityID, ResourceID: node, Action: "service.reload", ResourceType: "node", Reason: "queue fixture", TTL: time.Hour, SessionID: actor.principal.SessionID, RequestID: uuid.NewString(), RequestHash: hash, RequestSummary: summary, AuthorityResources: authority})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	// Hidden rows precede valid rows: permission filtering must happen before LIMIT.
	create(f.requester, f.workspace, nodeB, nodeB)
	create(f.requester, f.workspace, nodeA, nodeA, nodeB)
	create(f.requester, foreignWorkspace, foreignNode, foreignNode)
	own := create(f.approver, f.workspace, nodeA, nodeA)
	expired := create(f.requester, f.workspace, nodeA, nodeA)
	created, _ := value.FromTime(time.Now().Add(-2 * time.Hour))
	expires, _ := value.FromTime(time.Now().Add(-time.Hour))
	f.exec(`UPDATE approval_requests SET created_at=$1,expires_at=$2 WHERE id=$3`, `UPDATE approval_requests SET created_at=?,expires_at=? WHERE id=?`, created, expires, expired.ID)
	first := create(f.requester, f.workspace, nodeA, nodeA)
	second := create(f.requester, f.workspace, nodeA, nodeA)
	get := func(actor applyHTTPActor, query string) (items []approvals.Approval, more bool, cursor string) {
		t.Helper()
		w := f.call("GET", "/api/v1/approval-requests"+query, "", "", actor.cookie, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("queue: %d %s", w.Code, w.Body)
		}
		var page struct {
			Items []approvals.Approval `json:"items"`
			Page  struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items, page.Page.HasMore, page.Page.NextCursor
	}
	items, more, cursor := get(f.approver, "?page_size=1")
	if len(items) != 1 || items[0].ID != first.ID || !more || cursor != first.ID.String() {
		t.Fatalf("filtered first page: %+v %v %q", items, more, cursor)
	}
	if items[0].RequestHash != "" || len(items[0].RequestSummary) != 0 {
		t.Fatal("queue returned full approval content")
	}
	if _, err := f.s.approvals.Approve(t.Context(), approvals.Decision{ApprovalID: first.ID, ApproverID: f.approver.principal.IdentityID, SessionID: f.approver.principal.SessionID, Reason: "independent review", RequestID: uuid.NewString(), ExpectedRequestHash: first.RequestHash}); err != nil {
		t.Fatal(err)
	}
	items, more, cursor = get(f.approver, "?page_size=1&cursor="+cursor)
	if len(items) != 1 || items[0].ID != second.ID || more || cursor != "" {
		t.Fatalf("stable next page after decision: %+v %v %q", items, more, cursor)
	}
	items, _, _ = get(f.requester, "")
	if len(items) != 1 || items[0].ID != own.ID {
		t.Fatalf("self filter: %+v", items)
	}
	// Decision/consumption semantics are unchanged and decided rows leave the queue.
	hash, _ := approvals.GenericBinding(first.Action, first.ResourceType, first.ResourceID)
	err := database.Within(t.Context(), f.b, database.ReadCommitted, func(tx database.Tx) error {
		return approvals.ConsumeBoundTx(t.Context(), tx, first.ID, f.workspace, f.requester.principal.IdentityID, first.Action, first.ResourceType, first.ResourceID, hash)
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _, _ = get(f.approver, "")
	if len(items) != 1 || items[0].ID != second.ID {
		t.Fatalf("consumed/expired queue: %+v", items)
	}
	assertApplyHTTPProblem(t, f.call("GET", "/api/v1/approval-requests", "", "", f.reader.cookie, nil), 403, "forbidden")
	// New grants cannot confer authority over pre-existing requests.
	late, _ := value.FromTime(time.Now().Add(time.Minute))
	f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES($1,$2,$3,'SecurityAdmin','node',$4,$5)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_at) VALUES(?,?,?,'SecurityAdmin','node',?,?)`, uuid.Must(uuid.NewV7()), f.reader.principal.IdentityID, f.workspace, nodeA, late)
	items, _, _ = get(f.reader, "")
	if len(items) != 0 {
		t.Fatalf("post-snapshot grant exposed requests: %+v", items)
	}
	for _, query := range []string{"?page_size=0", "?page_size=201", "?page_size=abc", "?cursor=bad", "?cursor=" + uuid.NewString()} {
		w := f.call(http.MethodGet, "/api/v1/approval-requests"+query, "", "", f.approver.cookie, nil)
		if w.Code != 400 {
			t.Fatalf("invalid pagination %q: %d %s", query, w.Code, w.Body)
		}
	}
	// Selecting the other workspace returns only that workspace's requests.
	w := f.call("GET", "/api/v1/approval-requests", "", "", f.approver.cookie, func(r *http.Request) { r.Header.Set("X-Workspace-ID", foreignWorkspace.String()) })
	var foreign struct {
		Items []approvals.Approval `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &foreign) != nil || len(foreign.Items) != 1 || foreign.Items[0].WorkspaceID != foreignWorkspace {
		t.Fatalf("workspace filter: %d %s", w.Code, w.Body)
	}
	// A current role withdrawal removes the row, even though it existed at request time.
	f.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2 AND resource_id=$3`, `DELETE FROM role_bindings WHERE identity_id=? AND workspace_id=? AND resource_id=?`, f.approver.principal.IdentityID, f.workspace, nodeA)
	assertApplyHTTPProblem(t, f.call("GET", "/api/v1/approval-requests", "", "", f.approver.cookie, nil), 403, "forbidden")
	t.Log(fmt.Sprintf("bounded pending approval queue passed with runtime credentials (%T)", f.b))
}
