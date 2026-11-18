package useroperations

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/tracecontext"
	"github.com/google/uuid"
)

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestPolicyValidationDefinesBytesPeriodsAndUTC(t *testing.T) {
	nodeID := uuid.Must(uuid.NewV7())
	base := PolicyRequest{NodeID: nodeID, Username: "alice", QuotaPeriod: "monthly", QuotaDirection: "rxtx", QuotaBytes: 1024, IdempotencyKey: "policy-1", ActorID: "operator", Reason: "ticket", RequestID: "request-1", Traceparent: testTraceparent}
	utc := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	base.ExpiresAt = &utc
	if err := validatePolicyRequest(base); err != nil {
		t.Fatalf("valid policy: %v", err)
	}

	tests := []struct {
		name   string
		change func(*PolicyRequest)
	}{
		{"free quota must use none", func(value *PolicyRequest) { value.QuotaPeriod = "none" }},
		{"none quota must be zero", func(value *PolicyRequest) { value.QuotaPeriod, value.QuotaBytes = "none", 1 }},
		{"unknown direction", func(value *PolicyRequest) { value.QuotaDirection = "both-ish" }},
		{"unsafe JSON integer quota", func(value *PolicyRequest) { value.QuotaBytes = MaxSafeQuotaBytes + 1 }},
		{"invalid username", func(value *PolicyRequest) { value.Username = "alice/../../root" }},
		{"offset expiry", func(value *PolicyRequest) {
			expiry := time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))
			value.ExpiresAt = &expiry
		}},
		{"fractional expiry", func(value *PolicyRequest) { expiry := utc.Add(time.Nanosecond); value.ExpiresAt = &expiry }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.change(&value)
			if !errors.Is(validatePolicyRequest(value), ErrInvalidRequest) {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestQuotaDirectionAndMonthlyBoundary(t *testing.T) {
	if got := quotaValue("rx", 10, 20); got != 10 {
		t.Fatalf("rx=%d", got)
	}
	if got := quotaValue("tx", 10, 20); got != 20 {
		t.Fatalf("tx=%d", got)
	}
	if got := quotaValue("rxtx", 10, 20); got != 30 {
		t.Fatalf("rxtx=%d", got)
	}
	offset := time.FixedZone("minus-seven", -7*60*60)
	input := time.Date(2026, 9, 30, 20, 0, 0, 0, offset)
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := monthStart(input); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("month start=%s", got)
	}
}

func TestStableSchedulerIdentityAndConcurrencyLimit(t *testing.T) {
	first := operationIdempotencyKey("policy", "node", "alice", "2", "quota", "2026-08-01T00:00:00Z")
	// Captured from main at 5eb5896 before the port substitution.
	if first != "i14-d0bab4db91ed94274aca33de89ee0cc44448605f14c9ed502494599b22eb6f7d" || stableTraceparent(first) != "00-4cc2ffdfcb3701893e1ddaf39bd3e959-a48d3e519fe5231b-01" {
		t.Fatal("scheduler identity baseline changed")
	}
	if second := operationIdempotencyKey("policy", "node", "alice", "2", "quota", "2026-08-01T00:00:00Z"); first != second {
		t.Fatal("scheduler idempotency key changed")
	}
	if len(first) > 128 || tracecontext.TraceID(stableTraceparent(first)) == "" {
		t.Fatalf("invalid stable identities key=%q trace=%q", first, stableTraceparent(first))
	}
	if DefaultGlobalConcurrency != 50 || !strings.HasPrefix(first, "i14-") {
		t.Fatal("documented global concurrency or key namespace changed")
	}
}
