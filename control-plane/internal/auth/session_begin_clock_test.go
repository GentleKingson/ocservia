package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/database"
)

// Embedding leaves the unused Store methods unavailable: neither workflow
// should read its clock or reach a store when acquiring a transaction fails.
type unavailableSessionBackend struct {
	database.Backend
	err    error
	begins int
}

func (b *unavailableSessionBackend) Begin(context.Context, database.Isolation) (database.Tx, error) {
	b.begins++
	return nil, b.err
}

func TestSessionClockStartsAfterBegin(t *testing.T) {
	for _, name := range []string{"session", "break-glass"} {
		t.Run(name, func(t *testing.T) {
			unavailable := errors.New("test transaction acquisition failure")
			backend := &unavailableSessionBackend{err: unavailable}
			clockCalls := 0
			token := "test-offline-credential"
			digest := sha256.Sum256([]byte(token))
			s := &Service{
				backend:             backend,
				sessionTTL:          time.Hour,
				breakGlassEnabled:   true,
				breakGlassTokenHash: digest[:],
				now: func() time.Time {
					clockCalls++
					return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
				},
			}
			var err error
			if name == "session" {
				_, _, err = s.createSession(context.Background(), "test-issuer", "subject", "", "", false, nil)
			} else {
				_, _, err = s.BreakGlass(context.Background(), token, "test-request")
			}
			if !errors.Is(err, unavailable) || backend.begins != 1 {
				t.Fatalf("transaction acquisition: begins=%d err=%v", backend.begins, err)
			}
			if clockCalls != 0 {
				t.Fatalf("session TTL started before acquiring a transaction: clock calls=%d", clockCalls)
			}
		})
	}
}
