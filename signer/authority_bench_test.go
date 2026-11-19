package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func newBenchAuthorityAndCSR(tb testing.TB) (*authority, *x509.CertificateRequest) {
	tb.Helper()
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ocservia-bench-root"},
		NotBefore:             now,
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          []byte{1, 2, 3, 4},
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		tb.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		tb.Fatal(err)
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "ocservia-bench-intermediate"},
		NotBefore:             now,
		NotAfter:              now.Add(180 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          []byte{5, 6, 7, 8},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, rootCert, &caKey.PublicKey, rootKey)
	if err != nil {
		tb.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		tb.Fatal(err)
	}
	chainPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...,
	)
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(rootCert)
	intermediates.AddCert(caCert)
	auth := &authority{
		cert:          caCert,
		key:           caKey,
		chain:         chainPEM,
		id:            digest(caCert.Raw),
		roots:         roots,
		intermediates: intermediates,
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "alice"},
		DNSNames: []string{"alice.vpn.example"},
	}, leafKey)
	if err != nil {
		tb.Fatal(err)
	}
	csr, err := parseCSR(csrDER)
	if err != nil {
		tb.Fatal(err)
	}
	return auth, csr
}

func BenchmarkAuthorityIssue(b *testing.B) {
	auth, csr := newBenchAuthorityAndCSR(b)
	b.ReportAllocs()
	serial := int64(100)
	b.ResetTimer()
	for b.Loop() {
		serial++
		if _, err := auth.issue(csr, big.NewInt(serial)); err != nil {
			b.Fatal(err)
		}
	}
}
