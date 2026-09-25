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
	"fmt"
	"math/big"
	"net"
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
	a, err := loadAuthority(dir+"/ca.pem", dir+"/key.pem")
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{"vpn.example.test"}}, key)
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

func TestDurableProtocol(t *testing.T) {
	s, dir := fixture(t)
	id := uuid.New()
	csr := csrFixture(t, "alice")
	var wg sync.WaitGroup
	results := make(chan string, 20)
	for range 20 {
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
	for range 2 {
		if w := request(s, "/sign/revoke", body, map[string]string{"Idempotency-Key": id.String() + ":revoke"}); w.Code != 204 {
			t.Fatalf("revoke: %d", w.Code)
		}
	}
	if err := s.revoke(id.String(), record.Serial, "changed reason"); err != statusError(409) {
		t.Fatal("revoke conflict accepted", err)
	}
	if err := s.revoke(uuid.NewString(), record.Serial, "unknown"); err != statusError(404) {
		t.Fatal("unknown revoke accepted")
	}
	if err := s.revoke(id.String(), "999", "key compromise"); err != statusError(409) {
		t.Fatal("wrong serial accepted")
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
	crl.Reset()
	must(t, s.crl(&crl))
	block, _ = pem.Decode(crl.Bytes())
	next, err := x509.ParseRevocationList(block.Bytes)
	must(t, err)
	if next.Number.Cmp(list.Number) <= 0 {
		t.Fatal("CRL number did not advance")
	}
	must(t, s.backup(dir+"/backup.db"))
	must(t, s.db.Close())
	reopened, err := openStore(dir+"/ledger.db", s.ca, false)
	must(t, err)
	defer reopened.db.Close()
	r, err := reopened.sign(id, csr)
	must(t, err)
	if r.Chain != record.Chain || r.RevokedAt == nil {
		t.Fatal("restart lost revocation or result")
	}
	restored, err := openStore(dir+"/backup.db", s.ca, false)
	must(t, err)
	defer restored.db.Close()
	r, err = restored.sign(id, csr)
	must(t, err)
	if r.Chain != record.Chain || r.RevokedAt == nil {
		t.Fatal("backup lost revocation")
	}
	if _, err := openStore(dir+"/missing.db", s.ca, false); err == nil {
		t.Fatal("created missing ledger")
	}
	must(t, restored.db.Close())
	other, _ := fixture(t)
	if _, err := openStore(dir+"/backup.db", other.ca, false); err == nil {
		t.Fatal("CA replacement accepted")
	}
}

func TestCorruptionAndRestoreFloor(t *testing.T) {
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
	must(t, s.restore(dir+"/restored.db", int64(latest)))
	if err := s.restore(dir+"/restored.db", int64(latest)); err == nil {
		t.Fatal("existing state overwritten")
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("issued")).Put([]byte(id.String()), []byte(`{}`)) }))
	must(t, s.db.Close())
	if _, err := openStore(dir+"/ledger.db", s.ca, false); err == nil {
		t.Fatal("corruption accepted")
	}
}

func TestPolicyAndBounds(t *testing.T) {
	s, _ := fixture(t)
	id := uuid.NewString()
	csr := csrFixture(t, "alice")
	for _, tc := range []struct {
		path    string
		body    []byte
		headers map[string]string
		code    int
	}{
		{"/sign", nil, map[string]string{"Authorization": "Bearer wrong"}, 401},
		{"/unknown", nil, nil, 404},
		{"/sign", nil, map[string]string{"Content-Type": "text/plain"}, 415},
		{"/sign", bytes.Repeat([]byte("x"), 65537), nil, 413},
		{"/sign", []byte(`{}`), nil, 400},
		{"/sign/seal", []byte("secret"), map[string]string{"Content-Type": "application/octet-stream", "X-Ocservia-Node-ID": id, "X-Ocservia-Seal-Purpose": "user_password"}, 403},
		{"/sign/seal", bytes.Repeat([]byte("x"), 513), map[string]string{"Content-Type": "application/octet-stream"}, 413},
	} {
		if w := request(s, tc.path, tc.body, tc.headers); w.Code != tc.code {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.code)
		}
	}
	csr[len(csr)-1] ^= 1
	if signHTTP(s, id, csr).Code != 422 {
		t.Fatal("bad signature accepted")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	for _, template := range []*x509.CertificateRequest{
		{Subject: pkix.Name{CommonName: "alice"}, ExtraExtensions: []pkix.Extension{{Id: []int{2, 5, 29, 19}, Critical: true, Value: []byte{0x30, 3, 1, 1, 0xff}}}},
		{Subject: pkix.Name{CommonName: "alice"}, EmailAddresses: []string{"alice@example.test"}},
		{Subject: pkix.Name{CommonName: "alice", Organization: []string{"unexpected"}}},
	} {
		der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
		must(t, err)
		if signHTTP(s, uuid.NewString(), der).Code != 422 {
			t.Fatal("invalid CSR policy accepted")
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/sign", nil))
	if w.Code != 405 {
		t.Fatal("method accepted")
	}
	must(t, s.db.Close())
	if signHTTP(s, id, csrFixture(t, "bob")).Code != 503 || !s.failed.Load() {
		t.Fatal("storage failure did not fail closed")
	}
}

func approvedKeys(t *testing.T) (binding, []*rsa.PrivateKey) {
	t.Helper()
	b := binding{WorkspaceID: uuid.NewString(), NodeID: uuid.NewString(), EndpointID: strings.Repeat("a", 64), ApprovalID: uuid.NewString(), ApprovalHash: strings.Repeat("b", 64), ExportedAt: time.Now().UTC()}
	var private []*rsa.PrivateKey
	for _, use := range []string{"user_password", "certificate_p12_password"} {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		must(t, err)
		der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		must(t, err)
		b.Keys = append(b.Keys, descriptor{use, 1, use + "-key", digest(der), der})
		private = append(private, key)
	}
	return b, private
}

func TestTrustedSealing(t *testing.T) {
	s, dir := fixture(t)
	b, private := approvedKeys(t)
	must(t, s.importBinding(b, b))
	must(t, s.importBinding(b, b))
	for i, k := range b.Keys {
		w := request(s, "/sign/seal", []byte("unique-test-password"), map[string]string{"Content-Type": "application/octet-stream", "X-Ocservia-Node-ID": b.NodeID, "X-Ocservia-Seal-Purpose": k.Purpose})
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var response struct {
			Sealed  []byte `json:"sealed"`
			KeyID   string `json:"key_id"`
			Purpose string `json:"purpose"`
			Version int    `json:"version"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &response))
		plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, private[i], response.Sealed, nil)
		must(t, err)
		if string(plain) != "unique-test-password" || response.KeyID != k.KeyID || response.Purpose != k.Purpose || response.Version != 1 {
			t.Fatal("wrong sealing binding")
		}
		if _, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, private[1-i], response.Sealed, nil); err == nil {
			t.Fatal("cross-purpose decryption succeeded")
		}
		if _, err := s.seal(b.NodeID, k.Purpose, bytes.Repeat([]byte("x"), 191)); err != statusError(413) {
			t.Fatal("oversize RSA input accepted")
		}
	}
	for _, change := range []func(*binding){func(b *binding) { b.NodeID = uuid.NewString() }, func(b *binding) { b.EndpointID = strings.Repeat("c", 64) }, func(b *binding) { b.Keys[0].Purpose = b.Keys[1].Purpose }, func(b *binding) { b.Keys[0].KeyID = "wrong" }, func(b *binding) { b.Keys[0].Digest = strings.Repeat("c", 64) }} {
		bad := b
		bad.Keys = append([]descriptor(nil), b.Keys...)
		change(&bad)
		if s.importBinding(b, bad) == nil {
			t.Fatal("substituted export accepted")
		}
	}
	stale := b
	stale.ExportedAt = time.Now().Add(-time.Hour)
	if s.importBinding(stale, b) == nil {
		t.Fatal("stale export accepted")
	}
	must(t, s.disable(b.NodeID))
	if _, err := s.seal(b.NodeID, b.Keys[0].Purpose, []byte("password")); err != statusError(403) {
		t.Fatal("disabled binding used")
	}
	if s.importBinding(b, b) == nil {
		t.Fatal("disabled binding reactivated")
	}
	must(t, s.db.Close())
	s2, err := openStore(dir+"/ledger.db", s.ca, false)
	must(t, err)
	defer s2.db.Close()
	if _, err := s2.seal(b.NodeID, b.Keys[0].Purpose, []byte("password")); err != statusError(403) {
		t.Fatal("disabled binding lost on restart")
	}
}

func TestCrashRecovery(t *testing.T) {
	for _, stage := range []string{"prepare", "signed", "commit", "reply"} {
		t.Run(stage, func(t *testing.T) {
			s, dir := fixture(t)
			must(t, s.db.Close())
			der := csrFixture(t, "crash")
			must(t, os.WriteFile(dir+"/csr.der", der, 0600))
			id := uuid.NewString()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNER_CRASH_DIR="+dir, "SIGNER_CRASH_STAGE="+stage, "SIGNER_CRASH_ID="+id)
			err := cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 71 {
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
			if stage == "reply" {
				saved, err := os.ReadFile(dir + "/committed.pem")
				must(t, err)
				if string(saved) != r.Chain {
					t.Fatal("lost response changed result")
				}
			}
		})
	}
}

func TestCrashChild(t *testing.T) {
	dir := os.Getenv("SIGNER_CRASH_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}
	a, err := loadAuthority(dir+"/ca.pem", dir+"/key.pem")
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
			r, err := s.sign(uuid.MustParse(os.Getenv("SIGNER_CRASH_ID")), der)
			must(t, err)
			must(t, os.WriteFile(dir+"/committed.pem", []byte(r.Chain), 0600))
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
	b := binding{WorkspaceID: uuid.NewString(), NodeID: uuid.NewString(), EndpointID: strings.Repeat("a", 64), ApprovalID: uuid.NewString(), ApprovalHash: strings.Repeat("b", 64), ExportedAt: time.Now().UTC()}
	for i, use := range []string{"user_password", "certificate_p12_password"} {
		prefix := "user"
		id := "fixture-user-key"
		if i == 1 {
			prefix = "p12"
			id = "fixture-key"
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

// Disposable image-smoke material, never a production initialization command.
func TestImageFixture(t *testing.T) {
	dir := os.Getenv("SIGNER_IMAGE_FIXTURE_DIR")
	if dir == "" {
		t.Skip("image fixture generation only")
	}
	s, source := fixture(t)
	for name, original := range map[string]string{"issuer-chain.pem": "ca.pem", "issuer-key.pem": "key.pem", "tls-ca.pem": "ca.pem"} {
		data, err := os.ReadFile(filepath.Join(source, original))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, name), data, 0600))
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "signer"}, DNSNames: []string{"signer", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, s.ca.cert, key.Public(), s.ca.key)
	must(t, err)
	must(t, os.WriteFile(dir+"/tls-cert.pem", append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), s.ca.chain...), 0600))
	der, err = x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	must(t, os.WriteFile(dir+"/tls-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
	must(t, os.WriteFile(dir+"/api-token", []byte(s.token), 0600))
}

func TestImageBindingsFixture(t *testing.T) {
	dir := os.Getenv("SIGNER_IMAGE_BINDINGS_DIR")
	if dir == "" {
		t.Skip("image binding generation only")
	}
	b, _ := approvedKeys(t)
	public, err := json.Marshal(b)
	must(t, err)
	must(t, os.WriteFile(dir+"/keys-public.json", public, 0600))
	for i := range b.Keys {
		b.Keys[i].DER = nil
	}
	approved, err := json.Marshal(b)
	must(t, err)
	must(t, os.WriteFile(dir+"/keys-approved.json", approved, 0600))
	for name, value := range map[string]string{"node-id": b.NodeID, "workspace-id": b.WorkspaceID, "endpoint-id": b.EndpointID} {
		must(t, os.WriteFile(dir+"/"+name, []byte(value), 0600))
	}
}
