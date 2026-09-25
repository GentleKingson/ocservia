package certificates

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHTTPSignerDedicatedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"certificate_chain_pem":"fixture"}`)) }))
	defer server.Close()
	path := t.TempDir() + "/ca.pem"
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewHTTPSignerWithCA(server.URL+"/sign", "token", time.Second, path)
	if err != nil {
		t.Fatal(err)
	}
	req := SignRequest{CertificateID: uuid.New(), CSRDER: []byte("csr")}
	if _, err := s.Sign(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	defaultClient, err := NewHTTPSignerWithCA(server.URL, "token", time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	if defaultClient.client.Transport != nil {
		t.Fatal("default transport changed")
	}
	if _, err := defaultClient.Sign(context.Background(), req); err == nil {
		t.Fatal("private CA leaked into default trust")
	}
	transport := s.client.Transport.(*http.Transport)
	transport.CloseIdleConnections()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "wrong.example.test"
	if _, err := s.Sign(context.Background(), req); err == nil {
		t.Fatal("wrong SAN accepted")
	}
	transport.CloseIdleConnections()
	if transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("TLS verification weakened")
	}
	if _, err := NewHTTPSignerWithCA(server.URL, "token", time.Second, path+"missing"); err == nil {
		t.Fatal("missing CA accepted")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPSignerWithCA(server.URL, "token", time.Second, path); err == nil {
		t.Fatal("invalid CA accepted")
	}
}
