package certificates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
)

func TestHTTPSignerUsesFixedBoundedAuthenticatedEndpoints(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer signer-token" {
			t.Error("missing signer bearer credential")
		}
		switch r.URL.Path {
		case "/sign":
			if r.Header.Get("Idempotency-Key") != id.String() {
				t.Error("sign request is not certificate-idempotent")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"certificate_chain_pem": "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"})
		case "/sign/revoke":
			w.WriteHeader(http.StatusNoContent)
		case "/sign/seal":
			body, _ := io.ReadAll(r.Body)
			if string(body) != "one-time-password" || r.Header.Get("X-Ocservia-Node-ID") == "" || r.Header.Get("X-Ocservia-Seal-Purpose") != "certificate_p12_password" {
				t.Error("seal request did not bind the node and plaintext")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sealed": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 64))), "key_id": "node-key-v1", "version": 1, "purpose": "certificate_p12_password"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	signer, err := NewHTTPSigner(server.URL+"/sign", "signer-token", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	signer.client.Transport = server.Client().Transport
	if _, err := signer.Sign(context.Background(), SignRequest{CertificateID: id, CSRDER: []byte("public-csr")}); err != nil {
		t.Fatal(err)
	}
	if err := signer.Revoke(context.Background(), RevokeSignerRequest{CertificateID: id, SerialNumber: "42", Reason: "retired"}); err != nil {
		t.Fatal(err)
	}
	sealed, err := signer.Seal(context.Background(), id, agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, []byte("one-time-password"))
	if err != nil || len(sealed.GetCiphertext()) != 64 || sealed.GetKeyId() != "node-key-v1" || sealed.GetVersion() != agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1 || sealed.GetPurpose() != agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD {
		t.Fatalf("sealed=%+v err=%v", sealed, err)
	}
}

func TestHTTPSignerRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://pki.example.test/sign", "https://user@pki.example.test/sign", "https://pki.example.test/sign?target=other"} {
		if _, err := NewHTTPSigner(endpoint, "token", time.Second); err == nil {
			t.Fatalf("accepted unsafe signer endpoint %q", endpoint)
		}
	}
}
