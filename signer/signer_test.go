package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

var (
	sharedKeysOnce sync.Once
	sharedRSA      [3]*rsa.PrivateKey
)

func testRSAKey(t *testing.T, idx int) *rsa.PrivateKey {
	t.Helper()
	sharedKeysOnce.Do(func() {
		for i := range sharedRSA {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			must(t, err)
			sharedRSA[i] = k
		}
	})
	return sharedRSA[idx]
}

func openStore(path string, ca *authority, initialize bool) (*service, error) {
	return openLedger(path, ca, initialize, false)
}

func fixture(t *testing.T) (*service, string) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "offline root"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	must(t, err)
	root, err = x509.ParseCertificate(der)
	must(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "online intermediate"}, IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, NotBefore: root.NotBefore, NotAfter: root.NotAfter.Add(-time.Hour)}
	der, err = x509.CreateCertificate(rand.Reader, ca, root, key.Public(), rootKey)
	must(t, err)
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
	k, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	must(t, os.WriteFile(dir+"/ca.pem", chain, 0600))
	must(t, os.WriteFile(dir+"/key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}), 0600))
	a, err := loadAuthority(dir+"/ca.pem", dir+"/key.pem", defaultCertTTL)
	must(t, err)
	s, err := openStore(dir+"/ledger.db", a, true)
	must(t, err)
	s.token = strings.Repeat("t", 32)
	t.Cleanup(func() { s.db.Close() })
	return s, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func csrFixture(t *testing.T, cn string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{"vpn.example.test"}}, testRSAKey(t, 0))
	must(t, err)
	return der
}

func request(s *service, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func signHTTP(s *service, id string, csr []byte) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"certificate_id": id, "csr_der": base64.StdEncoding.EncodeToString(csr)})
	return request(s, "/sign", body, map[string]string{"Idempotency-Key": id})
}

func approvedKeys(t *testing.T) (binding, []*rsa.PrivateKey) {
	t.Helper()
	b := binding{WorkspaceID: uuid.NewString(), NodeID: uuid.NewString(), EndpointID: strings.Repeat("a", 64), ExportedAt: time.Now().UTC()}
	var private []*rsa.PrivateKey
	for i, use := range []string{"user_password", "certificate_p12_password"} {
		key := testRSAKey(t, i+1)
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		must(t, err)
		b.Keys = append(b.Keys, descriptor{use, 1, use + "-key", digest(der), der})
		private = append(private, key)
	}
	return b, private
}

func TestDurableProtocol(t *testing.T) {
	t.Parallel()
	s, dir := fixture(t)
	id := uuid.New()
	csr := csrFixture(t, "alice")
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := signHTTP(s, id.String(), csr)
			if w.Code != 200 {
				t.Errorf("sign: %d", w.Code)
			}
			results <- w.Body.String()
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for v := range results {
		if first == "" {
			first = v
		}
		if v != first {
			t.Fatal("concurrent results differ")
		}
	}
	if signHTTP(s, id.String(), csrFixture(t, "bob")).Code != 409 {
		t.Fatal("CSR conflict accepted")
	}
	record, err := s.sign(id, csr)
	must(t, err)
	body, _ := json.Marshal(map[string]string{"certificate_id": id.String(), "serial_number": record.Serial, "reason": "key compromise"})
	if w := request(s, "/sign/revoke", body, map[string]string{"Idempotency-Key": id.String() + ":revoke"}); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if err := s.revoke(id.String(), record.Serial, "changed reason"); err != statusError(409) {
		t.Fatal("revoke conflict accepted", err)
	}
	var crl bytes.Buffer
	must(t, s.crl(&crl))
	block, _ := pem.Decode(crl.Bytes())
	list, err := x509.ParseRevocationList(block.Bytes)
	must(t, err)
	must(t, list.CheckSignatureFrom(s.ca.cert))
	if len(list.RevokedCertificateEntries) != 1 {
		t.Fatal("CRL missing revocation")
	}
	must(t, s.backup(dir+"/backup.db"))
	must(t, s.db.Close())
	restored, err := openStore(dir+"/backup.db", s.ca, false)
	must(t, err)
	defer restored.db.Close()
	r, err := restored.sign(id, csr)
	must(t, err)
	if r.Chain != record.Chain || r.RevokedAt == nil {
		t.Fatal("backup lost revocation")
	}
}

func TestCorruptionAndRestoreFloor(t *testing.T) {
	t.Parallel()
	s, dir := fixture(t)
	id := uuid.New()
	der := csrFixture(t, "alice")
	r, err := s.sign(id, der)
	must(t, err)
	must(t, s.backup(dir+"/old.db"))
	must(t, s.revoke(id.String(), r.Serial, "compromised"))
	var latest uint64
	must(t, s.db.View(func(tx *bolt.Tx) error { latest = tx.Bucket([]byte("audit")).Sequence(); return nil }))
	old, err := openStore(dir+"/old.db", s.ca, false)
	must(t, err)
	defer old.db.Close()
	if err := old.restore(dir+"/restored.db", int64(latest)); err == nil {
		t.Fatal("old snapshot accepted")
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("issued")).Put([]byte(id.String()), []byte(`{}`)) }))
	must(t, s.db.Close())
	reopened, err := openLedger(dir+"/ledger.db", s.ca, false, true)
	must(t, err)
	defer reopened.db.Close()
	if err := reopened.verify(); !errors.Is(err, invalidState) {
		t.Fatalf("full verification did not catch corruption: %v", err)
	}
}

func TestPolicyBoundsAndSealing(t *testing.T) {
	t.Parallel()
	s, dir := fixture(t)
	id := uuid.NewString()
	csr := csrFixture(t, "alice")
	for _, tc := range []struct {
		path    string
		body    []byte
		headers map[string]string
		code    int
	}{
		{"/sign", nil, map[string]string{"Authorization": "Bearer wrong"}, 401},
		{"/sign", nil, map[string]string{"Content-Type": "text/plain"}, 415},
		{"/sign", []byte(`{}`), nil, 400},
		{"/sign/seal", []byte("secret"), map[string]string{"Content-Type": "application/octet-stream", "X-Ocservia-Node-ID": id, "X-Ocservia-Seal-Purpose": "user_password"}, 403},
	} {
		if w := request(s, tc.path, tc.body, tc.headers); w.Code != tc.code {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.code)
		}
	}
	csr[len(csr)-1] ^= 1
	if signHTTP(s, id, csr).Code != 422 {
		t.Fatal("bad signature accepted")
	}

	b, private := approvedKeys(t)
	must(t, s.importBinding(b, b))
	pubBody, _ := json.Marshal(map[string]string{"node_id": b.NodeID, "purpose": "user_password"})
	if w := request(s, "/sign/public-key", pubBody, nil); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public key read failed: %d", w.Code)
	}
	for i, k := range b.Keys {
		w := request(s, "/sign/seal", []byte("unique-test-password"), map[string]string{"Content-Type": "application/octet-stream", "X-Ocservia-Node-ID": b.NodeID, "X-Ocservia-Seal-Purpose": k.Purpose})
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var response struct {
			Sealed []byte `json:"sealed"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &response))
		plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, private[i], response.Sealed, nil)
		must(t, err)
		if string(plain) != "unique-test-password" {
			t.Fatal("wrong sealing decryption")
		}
		if _, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, private[1-i], response.Sealed, nil); err == nil {
			t.Fatal("cross-purpose decryption succeeded")
		}
	}
	must(t, s.disable(b.NodeID))
	if request(s, "/sign/public-key", pubBody, nil).Code != 403 {
		t.Fatal("disabled binding served")
	}
	if got, err := parseCertTTL("24h"); err != nil || got != minCertTTL {
		t.Fatalf("24h: %v %v", got, err)
	}
	if _, err := parseCertTTL("23h59m59s"); err == nil {
		t.Fatal("sub-minimum TTL accepted")
	}
	parsedCSR, err := parseCSR(csrFixture(t, "alice"))
	must(t, err)
	if _, err := s.ca.issue(parsedCSR, big.NewInt(18)); err != nil {
		t.Fatal(err)
	}
	if s.ca.canIssueAt(s.ca.cert.NotAfter.Add(-defaultCertTTL)) {
		t.Fatal("issuer expiry boundary admitted")
	}
	must(t, s.db.Close())
	reopened, err := openStore(dir+"/ledger.db", s.ca, false)
	must(t, err)
	defer reopened.db.Close()
	if _, err := reopened.seal(b.NodeID, b.Keys[0].Purpose, []byte("pw")); err != statusError(403) {
		t.Fatal("disabled binding survived restart")
	}
}

func TestCrashRecovery(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"prepare", "reply"} {
		t.Run(stage, func(t *testing.T) {
			s, dir := fixture(t)
			must(t, s.db.Close())
			der := csrFixture(t, "crash")
			must(t, os.WriteFile(dir+"/csr.der", der, 0600))
			id := uuid.NewString()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNER_CRASH_DIR="+dir, "SIGNER_CRASH_STAGE="+stage, "SIGNER_CRASH_ID="+id)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 71 {
				t.Fatalf("child did not reach %s: %v", stage, err)
			}
			restarted, err := openStore(dir+"/ledger.db", s.ca, false)
			must(t, err)
			defer restarted.db.Close()
			r, err := restarted.sign(uuid.MustParse(id), der)
			must(t, err)
			again, err := restarted.sign(uuid.MustParse(id), der)
			must(t, err)
			if r.Chain != again.Chain {
				t.Fatal("retry reissued")
			}
		})
	}
}

func TestCrashChild(t *testing.T) {
	dir := os.Getenv("SIGNER_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}
	a, err := loadAuthority(dir+"/ca.pem", dir+"/key.pem", defaultCertTTL)
	must(t, err)
	s, err := openStore(dir+"/ledger.db", a, false)
	must(t, err)
	s.boundary = func(stage string) {
		if stage != os.Getenv("SIGNER_CRASH_STAGE") {
			return
		}
		if stage == "reply" {
			s.boundary = nil
			der, err := os.ReadFile(dir + "/csr.der")
			must(t, err)
			_, err = s.sign(uuid.MustParse(os.Getenv("SIGNER_CRASH_ID")), der)
			must(t, err)
		}
		os.Exit(71)
	}
	der, err := os.ReadFile(dir + "/csr.der")
	must(t, err)
	_, err = s.sign(uuid.MustParse(os.Getenv("SIGNER_CRASH_ID")), der)
	must(t, err)
	t.Fatal("crash not reached")
}

// Invoked by the Rust adapter test, using node-generated public keys only.
func TestSealInteropHelper(t *testing.T) {
	dir := os.Getenv("SIGNER_INTEROP_DIR")
	if dir == "" {
		t.Skip("Rust interop helper only")
	}
	s, _ := fixture(t)
	b := binding{WorkspaceID: uuid.NewString(), NodeID: uuid.NewString(), EndpointID: strings.Repeat("a", 64), ExportedAt: time.Now().UTC()}
	for i, use := range []string{"user_password", "certificate_p12_password"} {
		prefix, id := "user", "fixture-user-key"
		if i == 1 {
			prefix, id = "p12", "fixture-key"
		}
		data, err := os.ReadFile(filepath.Join(dir, prefix+"-sealing-public.pem"))
		must(t, err)
		block, _ := pem.Decode(data)
		if block == nil {
			t.Fatal("invalid public key")
		}
		b.Keys = append(b.Keys, descriptor{use, 1, id, digest(block.Bytes), block.Bytes})
	}
	var exported binding
	script := os.Getenv("SIGNER_NODE_EXPORT_SCRIPT")
	if script == "" {
		t.Fatal("node export script required for interop")
	}
	cmd := exec.Command("python3", script, "--node", b.NodeID, "--endpoint", b.EndpointID, "--user-key", filepath.Join(dir, "user-sealing-private.pem"), "--user-key-id", "fixture-user-key", "--p12-key", filepath.Join(dir, "p12-sealing-private.pem"), "--p12-key-id", "fixture-key")
	data, err := cmd.Output()
	must(t, err)
	must(t, json.Unmarshal(data, &exported))
	must(t, s.importBinding(b, exported))
	for i, k := range b.Keys {
		response := request(s, "/sign/seal", []byte("unique-random-password-fixture"), map[string]string{"Content-Type": "application/octet-stream", "X-Ocservia-Node-ID": b.NodeID, "X-Ocservia-Seal-Purpose": k.Purpose})
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		var v struct {
			Sealed []byte `json:"sealed"`
		}
		must(t, json.Unmarshal(response.Body.Bytes(), &v))
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("signer-%d.bin", i)), v.Sealed, 0600))
	}
}
