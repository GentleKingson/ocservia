package certificates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
)

func TestProductionSignerIntegration(t *testing.T) {
	endpoint := os.Getenv("OCSERV_TEST_PRODUCTION_SIGNER_URL")
	if endpoint == "" {
		t.Skip("isolated production Signer image required")
	}
	token := os.Getenv("OCSERV_TEST_PRODUCTION_SIGNER_TOKEN")
	ca := os.Getenv("OCSERV_TEST_PRODUCTION_SIGNER_CA")
	s, err := NewHTTPSignerWithCA(endpoint, token, 5*time.Second, ca)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "image-client"}, DNSNames: []string{"vpn.example.test"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	req := SignRequest{CertificateID: uuid.New(), CSRDER: csr}
	result, err := s.Sign(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := validateSignedCertificate(csr, result.CertificateChainPEM, time.Now())
	if err != nil {
		t.Fatal("Controller rejected production certificate", err)
	}
	for range 2 {
		if err := s.Revoke(context.Background(), RevokeSignerRequest{CertificateID: req.CertificateID, SerialNumber: leaf.SerialNumber.String(), Reason: "image test"}); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := s.Sign(context.Background(), req)
	if err != nil || !bytes.Equal(replay.CertificateChainPEM, result.CertificateChainPEM) {
		t.Fatal("revoked certificate was not historically replayed", err)
	}
	wrong, err := NewHTTPSignerWithCA(endpoint, strings.Repeat("wrong", 8), time.Second, ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Sign(context.Background(), req); err == nil {
		t.Fatal("wrong token accepted")
	}
	node, err := uuid.Parse(os.Getenv("OCSERV_TEST_PRODUCTION_SIGNER_NODE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range []agentv1.SealedSecretPurpose{agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD} {
		sealed, err := s.Seal(context.Background(), node, use, []byte("image-fixture-password"))
		if err != nil {
			t.Fatal(err)
		}
		if sealed.KeyId != sealPurposeName(use)+"-key" || len(sealed.Ciphertext) != 256 {
			t.Fatal("wrong purpose key selected")
		}
	}
}
