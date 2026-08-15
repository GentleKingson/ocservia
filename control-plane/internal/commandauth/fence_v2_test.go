package commandauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type connectionFenceGolden struct {
	SignatureVersion      int64    `json:"signature_version"`
	DomainSeparatorASCII  string   `json:"domain_separator_ascii"`
	TestSeedHex           string   `json:"test_seed_hex"`
	PublicKeyHex          string   `json:"public_key_hex"`
	KeyID                 string   `json:"key_id"`
	FenceIDHex            string   `json:"fence_id_hex"`
	NodeIDHex             string   `json:"node_id_hex"`
	EndpointIDHex         string   `json:"endpoint_id_hex"`
	OwnerInstanceIDHex    string   `json:"owner_instance_id_hex"`
	OwnerIncarnation      uint64   `json:"owner_incarnation"`
	OwnerEpoch            uint64   `json:"owner_epoch"`
	ConnectionIDHex       string   `json:"connection_id_hex"`
	AuthorizationRevision uint64   `json:"authorization_revision"`
	Capabilities          []string `json:"capabilities"`
	LeaseUntilSeconds     int64    `json:"lease_until_seconds"`
	LeaseUntilNanos       uint32   `json:"lease_until_nanos"`
	IssuedAtSeconds       int64    `json:"issued_at_seconds"`
	IssuedAtNanos         uint32   `json:"issued_at_nanos"`
	ExpiresAtSeconds      int64    `json:"expires_at_seconds"`
	ExpiresAtNanos        uint32   `json:"expires_at_nanos"`
	CanonicalPreimageHex  string   `json:"canonical_preimage_hex"`
	SignatureHex          string   `json:"signature_hex"`
}

type fenceBindingGolden struct {
	SignatureVersion      int64  `json:"signature_version"`
	DomainSeparatorASCII  string `json:"domain_separator_ascii"`
	TestSeedHex           string `json:"test_seed_hex"`
	PublicKeyHex          string `json:"public_key_hex"`
	KeyID                 string `json:"key_id"`
	OperationKind         uint32 `json:"operation_kind"`
	OperationIDHex        string `json:"operation_id_hex"`
	FenceIDHex            string `json:"fence_id_hex"`
	NodeIDHex             string `json:"node_id_hex"`
	EndpointIDHex         string `json:"endpoint_id_hex"`
	OwnerInstanceIDHex    string `json:"owner_instance_id_hex"`
	OwnerIncarnation      uint64 `json:"owner_incarnation"`
	OwnerEpoch            uint64 `json:"owner_epoch"`
	ConnectionIDHex       string `json:"connection_id_hex"`
	AuthorizationRevision uint64 `json:"authorization_revision"`
	Capability            string `json:"capability"`
	IssuedAtSeconds       int64  `json:"issued_at_seconds"`
	IssuedAtNanos         uint32 `json:"issued_at_nanos"`
	ExpiresAtSeconds      int64  `json:"expires_at_seconds"`
	ExpiresAtNanos        uint32 `json:"expires_at_nanos"`
	CanonicalPreimageHex  string `json:"canonical_preimage_hex"`
	SignatureHex          string `json:"signature_hex"`
}

func fenceFixtureClaims(t *testing.T, vector *connectionFenceGolden) ConnectionFenceClaimsV2 {
	t.Helper()
	return ConnectionFenceClaimsV2{
		SignatureVersion: uint32(vector.SignatureVersion), KeyID: vector.KeyID,
		FenceID: testFixed16(t, vector.FenceIDHex), NodeID: testFixed16(t, vector.NodeIDHex),
		EndpointID: testFixed32(t, vector.EndpointIDHex), OwnerInstanceID: testFixed16(t, vector.OwnerInstanceIDHex),
		OwnerIncarnation: vector.OwnerIncarnation, OwnerEpoch: vector.OwnerEpoch,
		ConnectionID: testFixed16(t, vector.ConnectionIDHex), AuthorizationRevision: vector.AuthorizationRevision,
		Capabilities:      append([]string(nil), vector.Capabilities...),
		LeaseUntilSeconds: vector.LeaseUntilSeconds, LeaseUntilNanos: vector.LeaseUntilNanos,
		IssuedAtSeconds: vector.IssuedAtSeconds, IssuedAtNanos: vector.IssuedAtNanos,
		ExpiresAtSeconds: vector.ExpiresAtSeconds, ExpiresAtNanos: vector.ExpiresAtNanos,
	}
}

func TestConnectionFenceV2CanonicalAndGoSignatureMatchSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "connection-fence-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector connectionFenceGolden
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	seed := testFixed32(t, vector.TestSeedHex)
	signer := NewSignerFromSeed(seed)
	if hex.EncodeToString(signer.PublicKey()) != vector.PublicKeyHex || signer.KeyID() != vector.KeyID {
		t.Fatal("connection fence fixture key does not match seed")
	}
	claims := fenceFixtureClaims(t, &vector)
	canonical, err := CanonicalConnectionFenceV2(claims)
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
}

func TestFenceBindingV2CanonicalAndGoSignatureMatchSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fence-binding-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vector fenceBindingGolden
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	seed := testFixed32(t, vector.TestSeedHex)
	signer := NewSignerFromSeed(seed)
	if hex.EncodeToString(signer.PublicKey()) != vector.PublicKeyHex || signer.KeyID() != vector.KeyID {
		t.Fatal("fence binding fixture key does not match seed")
	}
	claims := FenceBindingClaimsV2{
		SignatureVersion: uint32(vector.SignatureVersion), KeyID: vector.KeyID,
		OperationKind: vector.OperationKind, OperationID: testFixed16(t, vector.OperationIDHex),
		FenceID: testFixed16(t, vector.FenceIDHex), NodeID: testFixed16(t, vector.NodeIDHex),
		EndpointID: testFixed32(t, vector.EndpointIDHex), OwnerInstanceID: testFixed16(t, vector.OwnerInstanceIDHex),
		OwnerIncarnation: vector.OwnerIncarnation, OwnerEpoch: vector.OwnerEpoch,
		ConnectionID: testFixed16(t, vector.ConnectionIDHex), AuthorizationRevision: vector.AuthorizationRevision,
		Capability:      vector.Capability,
		IssuedAtSeconds: vector.IssuedAtSeconds, IssuedAtNanos: vector.IssuedAtNanos,
		ExpiresAtSeconds: vector.ExpiresAtSeconds, ExpiresAtNanos: vector.ExpiresAtNanos,
	}
	canonical, err := CanonicalFenceBindingV2(claims)
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
}

// TestConnectionFenceV2NeverCrossValidatesV1 locks the frozen boundary
// between the V1 and V2 contract families: disjoint domain separators, no
// shared signing input, and structural rejection of V1-style claims. A V1
// session grant can never be reinterpreted as a fence, which is the
// contract-level half of failing V1 closed for fenced mutations.
func TestConnectionFenceV2NeverCrossValidatesV1(t *testing.T) {
	if string(sessionGrantV1Domain) == string(connectionFenceV2Domain) ||
		string(sessionGrantV1Domain) == string(fenceBindingV2Domain) ||
		string(artifactGrantV1Domain) == string(connectionFenceV2Domain) ||
		string(artifactGrantV1Domain) == string(fenceBindingV2Domain) {
		t.Fatal("V1 and V2 domain separators must be disjoint")
	}

	seed := testFixed32(t, "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	signer := NewSignerFromSeed(seed)
	claims := ConnectionFenceClaimsV2{
		SignatureVersion: ConnectionFenceV2SignatureVersion, KeyID: signer.KeyID(),
		FenceID: [16]byte{1}, NodeID: [16]byte{2}, EndpointID: [32]byte{3},
		OwnerInstanceID: [16]byte{4}, OwnerIncarnation: 11, OwnerEpoch: 7,
		ConnectionID: [16]byte{5}, AuthorizationRevision: 99,
		Capabilities:      []string{"ocserv.config.apply"},
		LeaseUntilSeconds: 1700000100, LeaseUntilNanos: 0,
		IssuedAtSeconds: 1700000000, IssuedAtNanos: 0,
		ExpiresAtSeconds: 1700000200, ExpiresAtNanos: 0,
	}
	canonical, err := CanonicalConnectionFenceV2(claims)
	if err != nil {
		t.Fatal(err)
	}

	// A signature made over any V1 domain never verifies the V2 canonical
	// bytes, and vice versa: the families share no signing input.
	v1Domains := [][]byte{authorizationV1Domain, sessionGrantV1Domain, artifactGrantV1Domain}
	for _, domain := range v1Domains {
		v1Style := append(append([]byte{}, domain...), canonical[len(connectionFenceV2Domain):]...)
		v1Signature := ed25519.Sign(signer.privateKey, v1Style)
		if ed25519.Verify(signer.PublicKey(), canonical, v1Signature) {
			t.Fatal("V1-domain signature must not verify V2 canonical bytes")
		}
		v2Signature := ed25519.Sign(signer.privateKey, canonical)
		if ed25519.Verify(signer.PublicKey(), v1Style, v2Signature) {
			t.Fatal("V2 signature must not verify V1-domain bytes")
		}
	}

	// Tampering with any bound owner or lease field invalidates the proof.
	tampered := claims
	tampered.OwnerEpoch = claims.OwnerEpoch + 1
	tamperedCanonical, err := CanonicalConnectionFenceV2(tampered)
	if err != nil {
		t.Fatal(err)
	}
	v2Signature := ed25519.Sign(signer.privateKey, canonical)
	if ed25519.Verify(signer.PublicKey(), tamperedCanonical, v2Signature) {
		t.Fatal("tampered owner epoch must not verify")
	}
}

// TestConnectionFenceV2ClaimsAreStrict locks the frozen claims invariants:
// epochs and revisions start above zero, capability sets are sorted and
// unique, the lease deadline is after issuance, and the proof outlives the
// lease deadline.
func TestConnectionFenceV2ClaimsAreStrict(t *testing.T) {
	base := ConnectionFenceClaimsV2{
		SignatureVersion: ConnectionFenceV2SignatureVersion, KeyID: "ed25519-sha256:00",
		FenceID: [16]byte{1}, NodeID: [16]byte{2}, EndpointID: [32]byte{3},
		OwnerInstanceID: [16]byte{4}, OwnerIncarnation: 11, OwnerEpoch: 7,
		ConnectionID: [16]byte{5}, AuthorizationRevision: 99,
		Capabilities:      []string{"ocserv.config.apply"},
		LeaseUntilSeconds: 1700000100, LeaseUntilNanos: 0,
		IssuedAtSeconds: 1700000000, IssuedAtNanos: 0,
		ExpiresAtSeconds: 1700000200, ExpiresAtNanos: 0,
	}
	invalid := []func(*ConnectionFenceClaimsV2){
		func(c *ConnectionFenceClaimsV2) { c.SignatureVersion = 0 },
		func(c *ConnectionFenceClaimsV2) { c.SignatureVersion = 2 },
		func(c *ConnectionFenceClaimsV2) { c.OwnerEpoch = 0 },
		func(c *ConnectionFenceClaimsV2) { c.AuthorizationRevision = 0 },
		func(c *ConnectionFenceClaimsV2) {
			c.Capabilities = []string{"ocserv.users.write", "ocserv.config.apply"}
		},
		func(c *ConnectionFenceClaimsV2) {
			c.Capabilities = []string{"ocserv.config.apply", "ocserv.config.apply"}
		},
		func(c *ConnectionFenceClaimsV2) { c.Capabilities = []string{""} },
		func(c *ConnectionFenceClaimsV2) { c.LeaseUntilSeconds = base.IssuedAtSeconds },
		func(c *ConnectionFenceClaimsV2) { c.LeaseUntilSeconds = base.ExpiresAtSeconds + 1 },
		func(c *ConnectionFenceClaimsV2) { c.ExpiresAtSeconds = base.IssuedAtSeconds },
	}
	for index, mutate := range invalid {
		claims := base
		mutate(&claims)
		if _, err := CanonicalConnectionFenceV2(claims); err == nil {
			t.Fatalf("case %d: invalid claims accepted", index)
		}
	}
	if _, err := CanonicalConnectionFenceV2(base); err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}
}

// TestFenceBindingV2ClaimsAreStrict locks the frozen binding invariants and
// the attempt-matching rule used to accept late legitimate results while
// quarantining results recorded under any other term.
func TestFenceBindingV2ClaimsAreStrict(t *testing.T) {
	base := FenceBindingClaimsV2{
		SignatureVersion: ConnectionFenceV2SignatureVersion, KeyID: "ed25519-sha256:00",
		OperationKind: 1, OperationID: [16]byte{6}, FenceID: [16]byte{1},
		NodeID: [16]byte{2}, EndpointID: [32]byte{3}, OwnerInstanceID: [16]byte{4},
		OwnerIncarnation: 11, OwnerEpoch: 7, ConnectionID: [16]byte{5},
		AuthorizationRevision: 99, Capability: "ocserv.users.write",
		IssuedAtSeconds: 1700000000, IssuedAtNanos: 0,
		ExpiresAtSeconds: 1700000200, ExpiresAtNanos: 0,
	}
	invalid := []func(*FenceBindingClaimsV2){
		func(c *FenceBindingClaimsV2) { c.OperationKind = 0 },
		func(c *FenceBindingClaimsV2) { c.OperationKind = 5 },
		func(c *FenceBindingClaimsV2) { c.OwnerEpoch = 0 },
		func(c *FenceBindingClaimsV2) { c.AuthorizationRevision = 0 },
		func(c *FenceBindingClaimsV2) { c.Capability = "" },
		func(c *FenceBindingClaimsV2) { c.ExpiresAtSeconds = base.IssuedAtSeconds },
	}
	for index, mutate := range invalid {
		claims := base
		mutate(&claims)
		if _, err := CanonicalFenceBindingV2(claims); err == nil {
			t.Fatalf("case %d: invalid binding accepted", index)
		}
	}
	if _, err := CanonicalFenceBindingV2(base); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}

	fence := ConnectionFenceClaimsV2{
		FenceID: base.FenceID, OwnerInstanceID: base.OwnerInstanceID,
		OwnerIncarnation: base.OwnerIncarnation, OwnerEpoch: base.OwnerEpoch,
		ConnectionID: base.ConnectionID, AuthorizationRevision: base.AuthorizationRevision,
	}
	if !base.MatchesFence(fence) {
		t.Fatal("binding must match its own fence term")
	}
	// Any drift in the recorded term — a different epoch from a later
	// takeover, a replaced connection, a new incarnation, or a reissued
	// fence identity — breaks the match.
	drifts := []func(*FenceBindingClaimsV2){
		func(c *FenceBindingClaimsV2) { c.OwnerEpoch = 8 },
		func(c *FenceBindingClaimsV2) { c.ConnectionID = [16]byte{9} },
		func(c *FenceBindingClaimsV2) { c.OwnerIncarnation = 12 },
		func(c *FenceBindingClaimsV2) { c.OwnerInstanceID = [16]byte{10} },
		func(c *FenceBindingClaimsV2) { c.FenceID = [16]byte{11} },
		func(c *FenceBindingClaimsV2) { c.AuthorizationRevision = 100 },
	}
	for index, mutate := range drifts {
		binding := base
		mutate(&binding)
		if binding.MatchesFence(fence) {
			t.Fatalf("case %d: binding matched a different term", index)
		}
	}
}
