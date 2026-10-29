package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

func TestLocalResetApprovalPrecheckBackendHTTPIntegration(t *testing.T) {
	b, owner := authenticationBackendFixtureWithIsolation(t, true)
	f := newApplyHTTPFixtureWithBackend(t, b, owner)
	name := "precheck-" + uuid.NewString()
	const password = "an independent local approval test password"
	if _, err := f.s.auth.BootstrapLocalAdmin(t.Context(), name, password, f.workspace, name+"-approver", "a separate local approval reviewer password"); err != nil {
		t.Fatal(err)
	}
	adminCookie, _, err := f.s.auth.AuthenticateLocal(t.Context(), name, password)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES($1,$2,$3,'Operator','workspace',$4)`, `INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,created_at) VALUES(?,?,?,'Operator','workspace',?)`, uuid.Must(uuid.NewV7()), f.requester.principal.IdentityID, f.workspace, value.Timestamp{Valid: true})
	// An identity without a Local credential must be hidden just like a missing ID.
	oidcID := uuid.Must(uuid.NewV7())
	f.exec(`INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES($1,'precheck-oidc',$2,$3,$4)`, `INSERT INTO identities(id,issuer,subject,created_at,updated_at) VALUES(?,'precheck-oidc',?,?,?)`, oidcID, oidcID.String(), value.Timestamp{Valid: true}, value.Timestamp{Valid: true})
	var deniedBody string
	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"local", f.reader.principal.IdentityID},
		{"missing", uuid.Must(uuid.NewV7())},
		{"oidc", oidcID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"action":"local_user.reset-password","resource_type":"local_user","resource_id":%q,"reason":"restore access","ttl_seconds":300}`, tc.id)
			w := f.call("POST", "/api/v1/approval-requests", body, "", f.requester.cookie, nil)
			assertApplyHTTPProblem(t, w, http.StatusForbidden, "forbidden")
			if deniedBody != "" && deniedBody != w.Body.String() {
				t.Fatalf("credential existence changed denial: %s vs %s", deniedBody, w.Body)
			}
			deniedBody = w.Body.String()
			var count int
			if err := f.row(`SELECT count(*) FROM approval_requests WHERE requester_id=$1`, `SELECT count(*) FROM approval_requests WHERE requester_id=?`, f.requester.principal.IdentityID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("denied request persisted an approval: %d %v", count, err)
			}
			w = f.call("POST", "/api/v1/approval-requests", body, "", adminCookie, nil)
			if tc.name == "local" {
				if w.Code != http.StatusCreated {
					t.Fatalf("authorized Local reset approval: %d %s", w.Code, w.Body)
				}
			} else {
				assertApplyHTTPProblem(t, w, http.StatusNotFound, "not-found")
			}
		})
	}
}
