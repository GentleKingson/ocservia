package transportevents

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

func TestLegacySyntheticPayloadHashMatchesRustGoldenVectors(t *testing.T) {
	node := make([]byte, 16)
	for index := range node {
		node[index] = byte(index)
	}
	envelope := agentv1.CommandEnvelope{NodeId: node, ExpectedRevision: 1}
	cases := []struct {
		message  string
		expected string
	}{
		{"", "2d6daaae892285c786fba378aff37d4d0436dae76699061500b548e939782433"},
		{"hello", "a7299856cb7fa4e266b1234614361677e1d1b2466608d014e71b4a547c804397"},
		{"你好", "10f1ae9994bde9ea69de8fb965bf5029e97ad8b1bf1f00834d47831f60adf38d"},
		{strings.Repeat("x", 4096), "d6f3a8f8d0fc7ccbbfe6fec9f93bcad544d98fec0f3501aeef8be2a5d2b78daa"},
	}
	for _, test := range cases {
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: test.message}}
		hash, err := legacySyntheticPayloadHash(&envelope)
		testutil.NoError(t, err)
		if hex.EncodeToString(hash[:]) != test.expected {
			t.Fatalf("hash for %d-byte echo = %x", len(test.message), hash)
		}
	}
	envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	hash, err := legacySyntheticPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "2e5b198f3c3a2718113a4dbf2a552c730ddede13567ee448c6118459ccfa0d98" {
		t.Fatalf("noop hash = %x, %v", hash, err)
	}
	envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: "hello"}}
	envelope.ExpectedRevision = 2
	hash, err = legacySyntheticPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "79659b4e1819080191867174096d8aa5d01a43cb634cab9c51b113391643c343" {
		t.Fatalf("revision hash = %x, %v", hash, err)
	}
	envelope.ExpectedRevision = 1
	for index := range envelope.NodeId {
		envelope.NodeId[index]++
	}
	hash, err = legacySyntheticPayloadHash(&envelope)
	if err != nil || hex.EncodeToString(hash[:]) != "a45222e4babe147a02b9274937f09c337ac56764a56c61a9ebfd901d4fec7afe" {
		t.Fatalf("node hash = %x, %v", hash, err)
	}
}

var v1DomainSeparator = []byte("ocservia.command.semantic-hash.v1\x00")

func computeV1Canonical(nodeID []byte, expectedRevision uint64, payloadKind uint32, canonicalPayload []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(v1DomainSeparator)
	h.Write(nodeID)
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], expectedRevision)
	h.Write(rev[:])
	var kind [4]byte
	binary.BigEndian.PutUint32(kind[:], payloadKind)
	h.Write(kind[:])
	h.Write(canonicalPayload)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func v1CanonicalPayload(payloadKind uint32, payload v1Payload) []byte {
	switch payloadKind {
	case 100, 112:
		return canonicalFixtureStrings(payload.SessionID, payload.BootID)
	case 105, 107:
		return nil
	case 108:
		return canonicalFixtureStrings(payload.Message)
	case 113:
		return canonicalFixtureStrings(payload.IP)
	default:
		panic("unsupported payload_kind in test")
	}
}

func canonicalFixtureStrings(values ...string) []byte {
	var out []byte
	for _, value := range values {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		out = append(out, length[:]...)
		out = append(out, value...)
	}
	return out
}

type v1Payload struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
	BootID    string `json:"boot_id"`
	IP        string `json:"ip"`
}

type v1Vector struct {
	Name                 string    `json:"name"`
	NodeIDHex            string    `json:"node_id_hex"`
	ExpectedRevision     uint64    `json:"expected_revision"`
	PayloadKind          uint64    `json:"payload_kind"`
	Payload              v1Payload `json:"payload"`
	CanonicalPreimageHex string    `json:"canonical_preimage_hex"`
	ExpectedSHA256       string    `json:"expected_sha256"`
}

func loadV1Fixture(t *testing.T) []v1Vector {
	t.Helper()
	candidates := []string{filepath.Join("..", "..", "..", "testdata", "semantic-payload-hash-v1.json")}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "testdata", "semantic-payload-hash-v1.json"))
	}
	var path string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			path = c
			break
		}
	}
	if path == "" {
		t.Fatalf("semantic payload hash v1 fixture not found relative to %s", candidates[0])
	}
	raw, err := os.ReadFile(path)
	testutil.NoError(t, err)
	var doc struct {
		Version int        `json:"version"`
		Vectors []v1Vector `json:"vectors"`
	}
	testutil.NoError(t, json.Unmarshal(raw, &doc))
	if doc.Version != 1 {
		t.Fatalf("unexpected fixture version %d", doc.Version)
	}
	return doc.Vectors
}

func v1EnvelopeFromPayload(nodeID []byte, expectedRevision uint64, payloadKind uint32, payload v1Payload) *agentv1.CommandEnvelope {
	envelope := &agentv1.CommandEnvelope{NodeId: nodeID, ExpectedRevision: expectedRevision}
	switch payloadKind {
	case 100:
		envelope.Payload = &agentv1.CommandEnvelope_SessionDisconnect{SessionDisconnect: &agentv1.SessionDisconnect{SessionId: payload.SessionID, BootId: payload.BootID}}
	case 105:
		envelope.Payload = &agentv1.CommandEnvelope_ServiceReload{ServiceReload: &agentv1.ServiceReload{}}
	case 107:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticNoop{SyntheticNoop: &agentv1.SyntheticNoop{}}
	case 108:
		envelope.Payload = &agentv1.CommandEnvelope_SyntheticEcho{SyntheticEcho: &agentv1.SyntheticEcho{Message: payload.Message}}
	case 112:
		envelope.Payload = &agentv1.CommandEnvelope_SessionTerminate{SessionTerminate: &agentv1.SessionTerminate{SessionId: payload.SessionID, BootId: payload.BootID}}
	case 113:
		envelope.Payload = &agentv1.CommandEnvelope_IpBanRemove{IpBanRemove: &agentv1.IpBanRemove{Ip: payload.IP}}
	default:
		panic("unsupported payload_kind in test")
	}
	return envelope
}

func v1EnvelopeFromVector(nodeID []byte, expectedRevision uint64, payloadKind uint32, message string) *agentv1.CommandEnvelope {
	return v1EnvelopeFromPayload(nodeID, expectedRevision, payloadKind, v1Payload{Message: message})
}

func TestCanonicalSemanticHashV1MatchesSharedFixture(t *testing.T) {
	for _, vector := range loadV1Fixture(t) {
		nodeID, err := hex.DecodeString(vector.NodeIDHex)
		testutil.NoError(t, err)
		payload := v1CanonicalPayload(uint32(vector.PayloadKind), vector.Payload)
		mirror := computeV1Canonical(nodeID, vector.ExpectedRevision, uint32(vector.PayloadKind), payload)
		if hex.EncodeToString(mirror[:]) != vector.ExpectedSHA256 {
			t.Fatalf("%s: mirror digest mismatch got %x want %s", vector.Name, mirror, vector.ExpectedSHA256)
		}
		envelope := v1EnvelopeFromPayload(nodeID, vector.ExpectedRevision, uint32(vector.PayloadKind), vector.Payload)
		got, err := semanticpayload.HashV1(envelope)
		testutil.NoError(t, err)
		if hex.EncodeToString(got[:]) != vector.ExpectedSHA256 {
			t.Fatalf("%s: production digest mismatch got %x want %s", vector.Name, got, vector.ExpectedSHA256)
		}
	}
}

func TestCanonicalSemanticHashV1ExcludesDeliveryMetadata(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	a := v1EnvelopeFromVector(node, 1, 108, "hello")
	a.MessageId, a.CommandId, a.IdempotencyKey = []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, []byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, []byte{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3}
	a.Sequence, a.Traceparent, a.ActorId, a.Reason = 42, "00-aaa", "alice", "ticket-1"
	a.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY
	b := v1EnvelopeFromVector(node, 1, 108, "hello")
	b.MessageId, b.CommandId, b.IdempotencyKey = []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, []byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}, []byte{7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}
	b.Sequence, b.Traceparent, b.ActorId, b.Reason = 99, "00-bbb", "bob", "ticket-2"
	b.DeliveryMode = agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_RECONCILE_ONLY
	hashA, errA := semanticpayload.HashV1(a)
	hashB, errB := semanticpayload.HashV1(b)
	if errA != nil || errB != nil || hashA != hashB {
		t.Fatalf("identical semantic inputs must hash equally: %v, %v", errA, errB)
	}
}

func TestCanonicalSemanticHashV1ChangesOnSemanticInput(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	nodeShifted := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	hello, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "hello"))
	testutil.NoError(t, err)
	for _, tc := range []struct {
		name string
		env  *agentv1.CommandEnvelope
	}{
		{"message", v1EnvelopeFromVector(node, 1, 108, "world")},
		{"node", v1EnvelopeFromVector(nodeShifted, 1, 108, "hello")},
		{"revision", v1EnvelopeFromVector(node, 2, 108, "hello")},
		{"kind", v1EnvelopeFromVector(node, 1, 107, "")},
	} {
		got, err := semanticpayload.HashV1(tc.env)
		if err != nil || got == hello {
			t.Fatalf("%s change must change hash (err=%v)", tc.name, err)
		}
	}
}

func TestCanonicalSemanticHashV1RejectsUnicodeNormalization(t *testing.T) {
	node := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	composed, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "\u00e9"))
	testutil.NoError(t, err)
	decomposed, err := semanticpayload.HashV1(v1EnvelopeFromVector(node, 1, 108, "e\u0301"))
	testutil.NoError(t, err)
	if composed == decomposed {
		t.Fatal("no Unicode normalization: different bytes must hash differently")
	}
}
