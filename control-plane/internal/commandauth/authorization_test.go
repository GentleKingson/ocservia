package commandauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type goldenDocument struct {
	Version      uint32         `json:"version"`
	TestSeedHex  string         `json:"test_seed_hex"`
	PublicKeyHex string         `json:"public_key_hex"`
	KeyID        string         `json:"key_id"`
	Vectors      []goldenVector `json:"vectors"`
}

type goldenVector struct {
	Name                     string `json:"name"`
	ProtocolVersion          string `json:"protocol_version"`
	CommandIDHex             string `json:"command_id_hex"`
	IdempotencyKeyHex        string `json:"idempotency_key_hex"`
	NodeIDHex                string `json:"node_id_hex"`
	OperationIDHex           string `json:"operation_id_hex"`
	ActorIdentity            string `json:"actor_identity"`
	Action                   string `json:"action"`
	RequiredCapability       string `json:"required_capability"`
	ApprovalIDHex            string `json:"approval_id_hex"`
	ApprovalRequestSHA256Hex string `json:"approval_request_sha256_hex"`
	ExpectedRevision         uint64 `json:"expected_revision"`
	SemanticHashVersion      uint32 `json:"semantic_hash_version"`
	SemanticPayloadSHA256Hex string `json:"semantic_payload_sha256_hex"`
	PayloadKind              uint32 `json:"payload_kind"`
	DeliveryMode             uint32 `json:"delivery_mode"`
	IssuedAtSeconds          int64  `json:"issued_at_seconds"`
	IssuedAtNanos            uint32 `json:"issued_at_nanos"`
	ExpiresAtSeconds         int64  `json:"expires_at_seconds"`
	ExpiresAtNanos           uint32 `json:"expires_at_nanos"`
	CanonicalPreimageHex     string `json:"canonical_preimage_hex"`
	SignatureHex             string `json:"signature_hex"`
}

func TestCanonicalV1AndGoSignaturesMatchSharedGoldenVectors(t *testing.T) {
	document := loadGoldenDocument(t)
	if document.Version != 1 || len(document.Vectors) < 2 {
		t.Fatalf("invalid command authorization fixture version=%d vectors=%d", document.Version, len(document.Vectors))
	}
	seed := testFixed32(t, document.TestSeedHex)
	signer := NewSignerFromSeed(seed)
	if got := hex.EncodeToString(signer.PublicKey()); got != document.PublicKeyHex {
		t.Fatalf("public key = %s, want %s", got, document.PublicKeyHex)
	}
	if signer.KeyID() != document.KeyID {
		t.Fatalf("key ID = %s, want %s", signer.KeyID(), document.KeyID)
	}
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			claims := claimsFromGolden(t, document.KeyID, vector)
			canonical, err := CanonicalV1(claims)
			if err != nil {
				t.Fatal(err)
			}
			signature := ed25519.Sign(signer.privateKey, canonical)
			if vector.CanonicalPreimageHex == "" || vector.SignatureHex == "" {
				t.Fatalf("populate fixture: canonical=%s signature=%s", hex.EncodeToString(canonical), hex.EncodeToString(signature))
			}
			if got := hex.EncodeToString(canonical); got != vector.CanonicalPreimageHex {
				t.Fatalf("canonical bytes mismatch\ngot  %s\nwant %s", got, vector.CanonicalPreimageHex)
			}
			if got := hex.EncodeToString(signature); got != vector.SignatureHex {
				t.Fatalf("Go signature mismatch\ngot  %s\nwant %s", got, vector.SignatureHex)
			}
		})
	}
}

func TestLoadSignerRejectsUnsafeKeyPaths(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".command-key-test-")
	if err != nil {
		t.Fatal(err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var seed [32]byte
	seed[0] = 7
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	keyPath := filepath.Join(directory, "controller.pem")
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSigner(keyPath)
	if err != nil {
		t.Fatalf("safe key was rejected: %v", err)
	}
	if !bytes.Equal(loaded.PublicKey(), privateKey.Public().(ed25519.PublicKey)) {
		t.Fatal("loaded public key does not match")
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(keyPath); err == nil {
		t.Fatal("world-readable private key was accepted")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(directory, "controller-link.pem")
	if err := os.Symlink(keyPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(linkPath); err == nil {
		t.Fatal("symlinked private key was accepted")
	}
	if err := os.Chmod(directory, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(keyPath); err == nil {
		t.Fatal("key under group-writable ancestry was accepted")
	}
}

func loadGoldenDocument(t *testing.T) goldenDocument {
	t.Helper()
	candidates := []string{filepath.Join("..", "..", "..", "testdata", "command-authorization-v1.json")}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "command-authorization-v1.json"))
	}
	for _, candidate := range candidates {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var document goldenDocument
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("parse %s: %v", candidate, err)
		}
		return document
	}
	t.Fatalf("command authorization golden fixture not found")
	return goldenDocument{}
}

func claimsFromGolden(t *testing.T, keyID string, vector goldenVector) ClaimsV1 {
	t.Helper()
	return ClaimsV1{
		AuthorizationVersion:  1,
		KeyID:                 keyID,
		ProtocolVersion:       vector.ProtocolVersion,
		CommandID:             testFixed16(t, vector.CommandIDHex),
		IdempotencyKey:        testFixed16(t, vector.IdempotencyKeyHex),
		NodeID:                testFixed16(t, vector.NodeIDHex),
		OperationID:           testFixed16(t, vector.OperationIDHex),
		ActorIdentity:         vector.ActorIdentity,
		Action:                vector.Action,
		RequiredCapability:    vector.RequiredCapability,
		ApprovalID:            testOptional16(t, vector.ApprovalIDHex),
		ApprovalRequestSHA256: testOptional32(t, vector.ApprovalRequestSHA256Hex),
		ExpectedRevision:      vector.ExpectedRevision,
		SemanticHashVersion:   vector.SemanticHashVersion,
		SemanticPayloadSHA256: testFixed32(t, vector.SemanticPayloadSHA256Hex),
		PayloadKind:           vector.PayloadKind,
		DeliveryMode:          vector.DeliveryMode,
		IssuedAtSeconds:       vector.IssuedAtSeconds,
		IssuedAtNanos:         vector.IssuedAtNanos,
		ExpiresAtSeconds:      vector.ExpiresAtSeconds,
		ExpiresAtNanos:        vector.ExpiresAtNanos,
	}
}

func testHex(t *testing.T, value string, size int) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != size {
		t.Fatalf("decode %d-byte hex %q: length=%d error=%v", size, value, len(decoded), err)
	}
	return decoded
}

func testFixed16(t *testing.T, value string) [16]byte {
	t.Helper()
	return [16]byte(testHex(t, value, 16))
}

func testFixed32(t *testing.T, value string) [32]byte {
	t.Helper()
	return [32]byte(testHex(t, value, 32))
}

func testOptional16(t *testing.T, value string) *[16]byte {
	t.Helper()
	if value == "" {
		return nil
	}
	fixed := testFixed16(t, value)
	return &fixed
}

func testOptional32(t *testing.T, value string) *[32]byte {
	t.Helper()
	if value == "" {
		return nil
	}
	fixed := testFixed32(t, value)
	return &fixed
}
