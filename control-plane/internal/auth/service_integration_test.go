package auth

import (
	"context"
	"errors"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"os"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBreakGlassAlertsAuditsAndRequiresRotationIntegration(t *testing.T) {
	url := os.Getenv("OCSERV_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OCSERV_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	testutil.NoError(t, err)
	defer pool.Close()
	workspaceID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'break glass',$2,now(),now())`, workspaceID, "break-glass-"+workspaceID.String()); err != nil {
		t.Fatal(err)
	}
	token := "offline-emergency-credential-with-high-entropy"
	service, err := NewBackend(postgres.WrapPool(pool), Config{LocalEnabled: true, SessionKey: make([]byte, 32), SessionTTL: time.Hour, BreakGlassEnabled: true, BreakGlassTokenHash: TokenHash(token)})
	testutil.NoError(t, err)
	cookie, principal, err := service.BreakGlass(ctx, token, "break-glass-request")
	testutil.NoError(t, err)
	if !principal.BreakGlass || !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("break-glass session is not hardened")
	}
	var bounded bool
	if err := pool.QueryRow(ctx, `SELECT expires_at-created_at=interval '15 minutes' FROM auth_sessions WHERE id=$1`, principal.SessionID).Scan(&bounded); err != nil || !bounded {
		t.Fatalf("break-glass TTL changed: %t %v", bounded, err)
	}
	var alerts, audits int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM security_alerts WHERE source_session_id=$1 AND severity='critical'),(SELECT count(*) FROM audit_events WHERE workspace_id=$2 AND source_session_id=$1 AND action='break_glass.use')`, principal.SessionID, workspaceID).Scan(&alerts, &audits); err != nil || alerts != 1 || audits != 1 {
		t.Fatalf("break-glass alert/audit = %d/%d, %v", alerts, audits, err)
	}
	if _, _, err := service.BreakGlass(ctx, token, "replay"); !errors.Is(err, ErrBreakGlassRotationDue) {
		t.Fatalf("credential reuse error = %v", err)
	}
}
