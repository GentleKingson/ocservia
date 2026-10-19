package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthorizationErrorStatus(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{rbac.ErrForbidden, http.StatusForbidden},
		{fmt.Errorf("scope: %w", rbac.ErrForbidden), http.StatusForbidden},
		{database.ErrNotFound, http.StatusNotFound},
		{errors.New("read role bindings: connection refused"), http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		(&Server{}).writeAuthorizationError(response, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil), test.err)
		if response.Code != test.want {
			t.Fatalf("%v: status=%d want=%d", test.err, response.Code, test.want)
		}
	}

	// A role lookup that cannot reach the database is not reported as a denial.
	pool, err := pgxpool.New(context.Background(), "postgres://ocservia@127.0.0.1:1/ocservia?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := &Server{rbac: rbac.NewBackend(postgres.WrapPool(pool))}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	request.Header.Set("X-Workspace-ID", uuid.Must(uuid.NewV7()).String())
	_, err = server.authorizeRoute(request, auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), Issuer: "integration"})
	response := httptest.NewRecorder()
	server.writeAuthorizationError(response, request, err)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Authorization unavailable") {
		t.Fatalf("unavailable role store: err=%v status=%d body=%s", err, response.Code, response.Body)
	}
}
