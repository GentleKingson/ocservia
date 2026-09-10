package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func numericFixture(summary string) ChainRecord {
	return ChainRecord{EventID: uuid.MustParse("00000000-0000-7000-8000-000000000022"), WorkspaceID: uuid.MustParse("00000000-0000-7000-8000-000000000021"), At: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), ActorType: "controller", ActorID: "legacy", Action: "legacy.event", ResourceType: "workspace", ResourceID: uuid.MustParse("00000000-0000-7000-8000-000000000021"), RequestID: "legacy-preflight", Result: "succeeded", AfterSummary: json.RawMessage(summary)}
}

func TestLegacyNumericPayloadRemainsIdentical(t *testing.T) {
	// This rounding is an existing v1 authentication limitation, not a claim
	// that distinct JSONB integers above 2^53 have distinct historical MACs.
	record := numericFixture(`{"z":9007199254740993,"scale":1.2300,"dup":0,"dup":2}`)
	payload, err := encodeChainPayload(nil, record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"previous":null,"event_id":"00000000-0000-7000-8000-000000000022","workspace_id":"00000000-0000-7000-8000-000000000021","occurred_at":"2026-08-12T00:00:00Z","actor_type":"controller","actor_id":"legacy","action":"legacy.event","resource_type":"workspace","resource_id":"00000000-0000-7000-8000-000000000021","request_id":"legacy-preflight","trace_id":"","result":"succeeded","reason":"","AfterSummary":{"dup":2,"scale":1.23,"z":9007199254740992}}`
	if !bytes.Equal(payload, []byte(want)) {
		t.Fatalf("changed legacy payload: %s", payload)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != "f46a3fafe28b4de3a11d51ed5d0eb207057648b26a29f5a0eae2b1e8dd7a7acf" {
		t.Fatal("changed legacy digest")
	}
}

func TestLargeJSONBNumericPayload(t *testing.T) {
	first, err := encodeChainPayload(nil, numericFixture(`{"n":1e1000,"duplicate":0,"duplicate":2}`))
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := encodeChainPayload(nil, numericFixture(`{"duplicate":2,"n":10e999}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, equivalent) {
		t.Fatal("large JSONB equivalent values have nondeterministic payloads")
	}
	second, err := encodeChainPayload(nil, numericFixture(`{"n":2e1000,"duplicate":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(first) == sha256.Sum256(second) {
		t.Fatal("distinct large numbers share a digest")
	}
	for _, invalid := range []string{`{"n":1e1000,"bad":"\u0000"}`, `{"n":1e1000`, `{"n":1e999999}`} {
		if _, err := encodeChainPayload(nil, numericFixture(invalid)); err == nil {
			t.Fatalf("accepted invalid JSONB: %s", invalid)
		}
	}
}
