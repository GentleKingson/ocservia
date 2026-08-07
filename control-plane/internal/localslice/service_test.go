package localslice

import (
	"encoding/json"
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"google.golang.org/protobuf/proto"
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

func TestEventNamesAcceptPathChanged(t *testing.T) {
	name, err := eventName(transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_PATH_CHANGED)
	if err != nil || name != "path_changed" {
		t.Fatalf("eventName(PATH_CHANGED) = %q, %v; want path_changed", name, err)
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
	thirtySeconds := uint32(30_000)
	if _, delay, err := normalizeScenario(Scenario{DelayMillis: &thirtySeconds}); err != nil || delay != thirtySeconds {
		t.Fatalf("30-second heartbeat delay = (%d, %v)", delay, err)
	}
	overLimit := uint32(30_001)
	if _, _, err := normalizeScenario(Scenario{DelayMillis: &overLimit}); err == nil {
		t.Fatal("delay above 30 seconds was accepted")
	}
}

func TestNormalizeConfigApplyResultRejectsUnprovedOutcomes(t *testing.T) {
	candidate := bytesOf(0x11)
	previous := bytesOf(0x22)
	envelope := &agentv1.CommandEnvelope{Payload: &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{
		CandidateHash: candidate, ExpectedCurrentHash: previous, DesiredRevision: 7,
	}}}
	valid := &agentv1.ConfigApplyResult{CandidateHash: candidate, PreviousHash: previous, ObservedHash: candidate, AppliedRevision: 7, Healthy: true}
	validBytes, err := proto.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if state, _, err := normalizeConfigApplyResult(envelope, "succeeded", validBytes); err != nil || state != "succeeded" {
		t.Fatalf("valid success state=%q err=%v", state, err)
	}
	invalid := []*agentv1.ConfigApplyResult{
		{},
		{CandidateHash: bytesOf(0x33), PreviousHash: previous, ObservedHash: candidate, AppliedRevision: 7, Healthy: true},
		{CandidateHash: candidate, PreviousHash: previous, ObservedHash: candidate, AppliedRevision: 6, Healthy: true},
		{CandidateHash: candidate, PreviousHash: previous, ObservedHash: candidate, AppliedRevision: 7},
		{CandidateHash: candidate, PreviousHash: previous, ObservedHash: previous, Healthy: true, RolledBack: true, FailedCritical: true, FailureCode: "health_check_failed"},
	}
	for index, outcome := range invalid {
		encoded, marshalErr := proto.Marshal(outcome)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, _, err := normalizeConfigApplyResult(envelope, "succeeded", encoded); err == nil {
			t.Fatalf("invalid outcome %d was accepted", index)
		}
	}
	for index, raw := range [][]byte{nil, {0xff}} {
		if _, _, err := normalizeConfigApplyResult(envelope, "succeeded", raw); err == nil {
			t.Fatalf("malformed outcome %d was accepted", index)
		}
	}
}

func TestNormalizeConfigApplyResultSupportsIdempotentTerminalStates(t *testing.T) {
	candidate := bytesOf(0x11)
	previous := bytesOf(0x22)
	envelope := &agentv1.CommandEnvelope{Payload: &agentv1.CommandEnvelope_ConfigApply{ConfigApply: &agentv1.ConfigApply{
		CandidateHash: candidate, ExpectedCurrentHash: previous, DesiredRevision: 7,
	}}}
	cases := []struct {
		want   string
		result *agentv1.ConfigApplyResult
	}{
		{"rolled_back", &agentv1.ConfigApplyResult{CandidateHash: candidate, PreviousHash: previous, ObservedHash: previous, Healthy: true, RolledBack: true, FailureCode: "health_check_failed"}},
		{"failed", &agentv1.ConfigApplyResult{CandidateHash: candidate, PreviousHash: previous, FailedCritical: true, FailureCode: "rollback_failed"}},
	}
	for _, test := range cases {
		encoded, err := proto.Marshal(test.result)
		if err != nil {
			t.Fatal(err)
		}
		state, _, err := normalizeConfigApplyResult(envelope, "succeeded", encoded)
		if err != nil || state != test.want {
			t.Fatalf("normalized state=%q want=%q err=%v", state, test.want, err)
		}
	}
}

func bytesOf(value byte) []byte {
	result := make([]byte, 32)
	for index := range result {
		result[index] = value
	}
	return result
}
