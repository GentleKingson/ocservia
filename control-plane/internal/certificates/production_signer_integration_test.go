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
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
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
	testutil.NoError(t, err)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	testutil.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "image-client"}, DNSNames: []string{"vpn.example.test"}}, key)
	testutil.NoError(t, err)
	req := SignRequest{CertificateID: uuid.New(), CSRDER: csr}
	result, err := s.Sign(context.Background(), req)
	testutil.NoError(t, err)
	leaf, err := validateSignedCertificate(csr, result.CertificateChainPEM, time.Now())
	if err != nil {
		t.Fatal("Controller rejected production certificate", err)
	}
	for range 2 {
		testutil.NoError(t, s.Revoke(context.Background(), RevokeSignerRequest{CertificateID: req.CertificateID, SerialNumber: leaf.SerialNumber.String(), Reason: "image test"}))
	}
	replay, err := s.Sign(context.Background(), req)
	if err != nil || !bytes.Equal(replay.CertificateChainPEM, result.CertificateChainPEM) {
		t.Fatal("revoked certificate was not historically replayed", err)
	}
	wrong, err := NewHTTPSignerWithCA(endpoint, strings.Repeat("wrong", 8), time.Second, ca)
	testutil.NoError(t, err)
	if _, err := wrong.Sign(context.Background(), req); err == nil {
		t.Fatal("wrong token accepted")
	}
	node, err := uuid.Parse(os.Getenv("OCSERV_TEST_PRODUCTION_SIGNER_NODE"))
	testutil.NoError(t, err)
	for _, use := range []agentv1.SealedSecretPurpose{agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD} {
		sealed, err := s.Seal(context.Background(), node, use, []byte("image-fixture-password"))
		testutil.NoError(t, err)
		if sealed.KeyId != sealPurposeName(use)+"-key" || len(sealed.Ciphertext) != 256 {
			t.Fatal("wrong purpose key selected")
		}
	}
}
