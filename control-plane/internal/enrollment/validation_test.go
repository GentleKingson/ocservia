package enrollment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestValidShortRejectsWhitespaceOutsideContractLimit(t *testing.T) {
	if validShort("value ", 5) {
		t.Fatal("expected trailing whitespace to count toward the contract limit")
	}
	if validShort(" "+strings.Repeat("x", 5), 5) {
		t.Fatal("expected leading whitespace to be rejected")
	}
	if !validShort("value", 5) {
		t.Fatal("expected an exact-length value to be accepted")
	}
	if !validShort(strings.Repeat("界", 5), 5) {
		t.Fatal("expected the contract limit to count Unicode characters")
	}
	if validShort(strings.Repeat("界", 6), 5) {
		t.Fatal("expected an over-limit Unicode value to be rejected")
	}
}

func TestStoredValuesRejectSurroundingWhitespace(t *testing.T) {
	service := &Service{}
	nodeID := uuid.Must(uuid.NewV7())
	revocation := Revocation{NodeID: nodeID, ActorID: "operator", Reason: "revoke ", RequestID: uuid.Must(uuid.NewV7()).String()}
	if _, err := service.Revoke(context.Background(), revocation); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected an invalid revocation reason, got %v", err)
	}
}
