package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOptionalLocalAuthenticationAndValidation(t *testing.T) {
	ctx := context.Background()
	s, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{LocalEnabled: false, SessionKey: make([]byte, 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateLocal(ctx, "alice", "password"); !errors.Is(err, ErrLocalDisabled) {
		t.Fatalf("disabled local login: %v", err)
	}
	if _, err := s.CreateLocalCredential(ctx, "alice", "password"); !errors.Is(err, ErrLocalDisabled) {
		t.Fatalf("disabled local provisioning: %v", err)
	}
	if _, err := NewBackend(postgres.WrapPool(&pgxpool.Pool{}), Config{LocalEnabled: true, SessionKey: make([]byte, 31), SessionTTL: time.Hour}); err == nil {
		t.Fatal("weak session key accepted")
	}
}

func TestValidBreakGlassTokenIsOnlyCredentialAdmission(t *testing.T) {
	service := &Service{breakGlassEnabled: true, breakGlassTokenHash: TokenHash("correct")}
	if !service.ValidBreakGlassToken("correct") || service.ValidBreakGlassToken("wrong") {
		t.Fatal("incorrect emergency credential admission")
	}
	service.breakGlassEnabled = false
	if service.ValidBreakGlassToken("correct") {
		t.Fatal("disabled emergency credential accepted")
	}
}
