package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Historical approval rows remain storage records; no approval API is exposed.
func TestPendingApprovalQueueBackendIntegration(t *testing.T) {
	f := newApplyHTTPFixture(t)
	for _, actor := range []applyHTTPActor{f.requester, f.reader} {
		for _, path := range []string{"/api/v1/approval-requests", "/api/v1/approval-requests/" + uuid.Must(uuid.NewV7()).String()} {
			w := f.call(http.MethodGet, path, "", "", actor.cookie, nil)
			if w.Code != http.StatusNotFound {
				t.Fatalf("retired approval route: %d %s", w.Code, w.Body)
			}
		}
	}
}
