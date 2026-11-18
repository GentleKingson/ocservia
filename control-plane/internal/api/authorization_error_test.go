package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthorizationErrorStatus(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{auth.ErrForbidden, http.StatusForbidden},
		{fmt.Errorf("scope: %w", auth.ErrForbidden), http.StatusForbidden},
		{database.ErrNotFound, http.StatusNotFound},
		{errors.New("read workspace admins: connection refused"), http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		(&Server{}).writeAuthorizationError(response, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil), test.err)
		if response.Code != test.want {
			t.Fatalf("%v: status=%d want=%d", test.err, response.Code, test.want)
		}
	}

	// A workspace lookup that cannot reach the database is not reported as a denial.
	pool, err := pgxpool.New(context.Background(), "postgres://ocservia@127.0.0.1:1/ocservia?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	authSvc, err := auth.NewBackend(postgres.WrapPool(pool), auth.Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{auth: authSvc}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	request.Header.Set("X-Workspace-ID", uuid.Must(uuid.NewV7()).String())
	_, err = server.authorizeRoute(request, auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), Issuer: auth.LocalIssuer})
	response := httptest.NewRecorder()
	server.writeAuthorizationError(response, request, err)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Authorization unavailable") {
		t.Fatalf("unavailable workspace store: err=%v status=%d body=%s", err, response.Code, response.Body)
	}
}
