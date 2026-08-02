package localslice

import (
	"encoding/json"
	"strings"
	"testing"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

func TestOperationJSONOmitsAbsentIdentifiers(t *testing.T) {
	payload, err := json.Marshal(Operation{ID: "019cf000-0000-7000-8000-000000000001", State: "draft"})
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	for _, field := range []string{"node_id", "command_id"} {
		if strings.Contains(string(payload), field) {
			t.Fatalf("absent %s was serialized: %s", field, payload)
		}
	}
}

func TestValidTraceparent(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if !validTraceparent(valid) {
		t.Fatal("validTraceparent rejected a valid value")
	}
	for _, value := range []string{
		"", "00-0123-0123-01",
		"00-00000000000000000000000000000000-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",
		"00-0123456789abcdef0123456789abcdef-0123456789abcdef-zz",
		"00-0123456789ABCDEF0123456789abcdef-0123456789abcdef-01",
	} {
		if validTraceparent(value) {
			t.Fatalf("validTraceparent accepted %q", value)
		}
	}
}

func TestEventNamesRejectUnspecified(t *testing.T) {
	if _, err := eventName(transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_UNSPECIFIED); err == nil {
		t.Fatal("eventName accepted an unspecified event")
	}
}

func TestScenarioDefaultsPreservePresence(t *testing.T) {
	heartbeats, delay, err := normalizeScenario(Scenario{})
	if err != nil || heartbeats != 3 || delay != 100 {
		t.Fatalf("defaults = (%d, %d, %v)", heartbeats, delay, err)
	}
	zero := uint32(0)
	if _, _, err := normalizeScenario(Scenario{HeartbeatCount: &zero}); err == nil {
		t.Fatal("explicit zero heartbeat_count was accepted")
	}
	if heartbeats, delay, err := normalizeScenario(Scenario{DelayMillis: &zero}); err != nil || heartbeats != 3 || delay != 0 {
		t.Fatalf("explicit zero delay = (%d, %d, %v)", heartbeats, delay, err)
	}
}
