package certificates

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	certificatestore "github.com/GentleKingson/ocservia/control-plane/internal/certificates/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidateSignedCertificateRequiresVerifiedClosedChain(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Certificate Validation Root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node.example.test"}, DNSNames: []string{"node.example.test"}}, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: csr.Subject, DNSNames: csr.DNSNames, NotBefore: now.Add(5 * time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	if _, err := validateSignedCertificate(csrDER, chain, now); err != nil {
		t.Fatalf("valid chain: %v", err)
	}
	if _, err := validateSignedCertificate(csrDER, chain, now.Add(-time.Second)); err == nil {
		t.Fatal("accepted leaf more than five minutes ahead")
	}
	if _, err := validateSignedCertificate(csrDER, chain, leafTemplate.NotAfter); err == nil {
		t.Fatal("accepted expired leaf")
	}
	serverOnly := *leafTemplate
	serverOnly.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	serverDER, err := x509.CreateCertificate(rand.Reader, &serverOnly, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	serverChain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	if _, err := validateSignedCertificate(csrDER, serverChain, now); err == nil {
		t.Fatal("accepted server-only leaf")
	}
	withPrivateKey := append(append([]byte(nil), chain...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})...)
	if _, err := validateSignedCertificate(csrDER, withPrivateKey, now); err == nil {
		t.Fatal("accepted trailing private key")
	}
	forgedLeaf := append([]byte(nil), leafDER...)
	forgedLeaf[len(forgedLeaf)-1] ^= 0x01
	forgedChain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: forgedLeaf}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	if _, err := validateSignedCertificate(csrDER, forgedChain, now); err == nil {
		t.Fatal("accepted invalid leaf signature")
	}
}

func TestPendingRevocationRequiresOriginalAuthorization(t *testing.T) {
	signer, err := commandauth.NewRandomSigner()
	if err != nil {
		t.Fatal(err)
	}
	s := Service{grantSigner: signer}
	item := certificatestore.PendingRevocation{OperationID: uuid.New(), CommandID: uuid.New(), NodeID: uuid.New()}
	certificateID := uuid.Must(uuid.NewV7())
	now := time.Now()
	envelope := &agentv1.CommandEnvelope{
		ProtocolVersion: commandauth.ProtocolVersion,
		NodeId:          item.NodeID[:], OperationId: item.OperationID[:], CommandId: item.CommandID[:],
		IdempotencyKey: item.OperationID[:], ExpectedRevision: 1,
		Action: "certificate.revoke", RequiredCapability: "ocserv.certificate.revoke",
		ActorId: uuid.NewString(), IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(time.Minute)),
		DeliveryMode: agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_EXECUTE_OR_REPLAY,
		Payload: &agentv1.CommandEnvelope_CertificateRevoke{CertificateRevoke: &agentv1.CertificateRevoke{
			CertificateId: certificateID[:], CertificateVersion: 1, Reason: "rotate certificate",
		}},
	}
	if err := semanticpayload.PopulateV2(envelope); err != nil {
		t.Fatal(err)
	}
	if err := signer.Authorize(envelope); err != nil {
		t.Fatal(err)
	}
	item.Envelope, err = proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, gotID, err := s.authorizedRevocation(item); gotID != certificateID || err != nil {
		t.Fatalf("original revocation rejected: ID=%s error=%v", gotID, err)
	}

	envelope.GetCertificateRevoke().Reason = "substituted reason"
	for _, rewriteDigest := range []bool{false, true} {
		if rewriteDigest {
			if err := semanticpayload.PopulateV2(envelope); err != nil {
				t.Fatal(err)
			}
		}
		item.Envelope, err = proto.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.authorizedRevocation(item); err == nil {
			t.Fatalf("changed revocation accepted with recomputed digest=%v", rewriteDigest)
		}
	}
}
