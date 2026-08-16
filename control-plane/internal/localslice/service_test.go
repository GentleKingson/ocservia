package localslice

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestResultCommitBarrierSignalsBeforeRelease(t *testing.T) {
	directory := t.TempDir()
	commandID, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "arm"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(&agentv1.CommandResult{CommandId: commandID[:]})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{resultCommitBarrierDir: directory}
	done := make(chan error, 1)
	go func() {
		done <- service.waitAtResultCommitBarrier(context.Background(), &transportv1.TransportEvent{
			Type:    transportv1.TransportEventType_TRANSPORT_EVENT_TYPE_COMMAND_RESULT,
			Payload: payload,
		}, time.Unix(123, 0))
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if received, readErr := os.ReadFile(filepath.Join(directory, "received")); readErr == nil {
			if !strings.HasPrefix(string(received), commandID.String()+"\n1970-01-01T00:02:03Z") {
				t.Fatalf("unexpected barrier signal %q", received)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("result barrier did not signal receipt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("barrier returned before release: %v", err)
	default:
	}
	if err := os.WriteFile(filepath.Join(directory, "release"), []byte(commandID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("result barrier did not release")
	}
}

func TestCertificateCSRResultRejectsSubjectSubstitution(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "attacker.example.test"}, DNSNames: []string{"attacker.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(publicDER)
	id := []byte("0123456789abcdef")
	envelope := &agentv1.CommandEnvelope{Payload: &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{CertificateId: id, CommonName: "node.example.test", DnsNames: []string{"node.example.test"}, KeyBits: 2048}}}
	encoded, err := proto.Marshal(&agentv1.CertificateCsrResult{CertificateId: id, CsrDer: csrDER, PublicKeySha256: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeCertificateCSRResult(envelope, "succeeded", encoded); err == nil {
		t.Fatal("substituted CSR subject was accepted")
	}
}

func TestMatchingAttackerCSRWithoutPrivdReceiptCannotBecomeReady(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certificateID, nodeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	commandID, operationID, idempotencyID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node.example.test"}, DNSNames: []string{"node.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicDigest := sha256.Sum256(publicDER)
	semantic := bytesOf(0x51)
	envelope := &agentv1.CommandEnvelope{
		CommandId: commandID[:], OperationId: operationID[:], IdempotencyKey: idempotencyID[:], NodeId: nodeID[:],
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		SemanticPayloadSha256:      semantic,
		Payload: &agentv1.CommandEnvelope_CertificateCsr{CertificateCsr: &agentv1.CertificateCsr{
			CertificateId: certificateID[:], CommonName: "node.example.test", DnsNames: []string{"node.example.test"}, KeyBits: 2048,
		}},
	}
	encoded, err := proto.Marshal(&agentv1.CertificateCsrResult{CertificateId: certificateID[:], CsrDer: csrDER, PublicKeySha256: publicDigest[:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeCertificateCSRResult(envelope, "succeeded", encoded); err != nil {
		t.Fatalf("matching attacker CSR should pass only the structural check: %v", err)
	}
	accepted := time.Unix(1_700_000_000, 0).UTC()
	result := &agentv1.CommandResult{
		CommandId: commandID[:], IdempotencyKey: idempotencyID[:], PayloadSha256: semantic,
		SemanticPayloadHashVersion: agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2,
		State:                      agentv1.CommandResultState_COMMAND_RESULT_STATE_SUCCEEDED, Result: encoded,
		AcceptedAt: timestamppb.New(accepted), CompletedAt: timestamppb.New(accepted.Add(time.Second)),
	}
	verification := privdattestation.VerifyResult(context.Background(), nil, nodeID, envelope, result)
	if verification.Status != "missing" || verification.FailureReason != "receipt_missing" {
		t.Fatalf("missing root receipt verification = %+v", verification)
	}
	if csr, err := normalizeCertificateCSRResult(envelope, "unknown", encoded); err != nil || csr != nil {
		t.Fatalf("unattested CSR was eligible for csr_ready: csr=%v err=%v", csr != nil, err)
	}
}

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
