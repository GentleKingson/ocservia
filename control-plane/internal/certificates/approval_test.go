package certificates

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestCertificateActionApprovalBindsExactExportAndRevokeContent(t *testing.T) {
	certificateID := uuid.MustParse("01900000-0000-7000-8000-000000000101")
	nodeID := uuid.MustParse("01900000-0000-7000-8000-000000000102")
	artifactID := uuid.MustParse("01900000-0000-7000-8000-000000000103")
	exportHash, summary, err := certificateActionBinding("certificate.private_key.export", certificateID, nodeID, 7, "certificate_p12", artifactID, "serial-1", []byte("certificate-chain"))
	if err != nil || len(summary) == 0 {
		t.Fatalf("export binding err=%v summary=%s", err, summary)
	}
	tamperedVersion, _, _ := certificateActionBinding("certificate.private_key.export", certificateID, nodeID, 8, "certificate_p12", artifactID, "serial-1", []byte("certificate-chain"))
	tamperedArtifact, _, _ := certificateActionBinding("certificate.private_key.export", certificateID, nodeID, 7, "certificate_p12", uuid.Must(uuid.NewV7()), "serial-1", []byte("certificate-chain"))
	tamperedSerial, _, _ := certificateActionBinding("certificate.private_key.export", certificateID, nodeID, 7, "certificate_p12", artifactID, "serial-2", []byte("certificate-chain"))
	if bytes.Equal(exportHash, tamperedVersion) || bytes.Equal(exportHash, tamperedArtifact) || bytes.Equal(exportHash, tamperedSerial) {
		t.Fatal("export approval hash did not bind version, artifact identity, and certificate serial")
	}
	revokeHash, _, err := certificateActionBinding("certificate.revoke", certificateID, nodeID, 7, "key compromise", uuid.Nil, "serial-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tamperedReason, _, _ := certificateActionBinding("certificate.revoke", certificateID, nodeID, 7, "retired", uuid.Nil, "serial-1", nil)
	if bytes.Equal(revokeHash, tamperedReason) || bytes.Equal(revokeHash, exportHash) {
		t.Fatal("certificate actions or revoke reason were not independently bound")
	}
}
