package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"strings"
	"time"
)

const policy = "rsa-client-v1-24h"

type authority struct {
	cert  *x509.Certificate
	key   crypto.Signer
	chain []byte
	id    string
}

func digest(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }

func loadAuthority(chainFile, keyFile string) (*authority, error) {
	chain, err := os.ReadFile(chainFile)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for rest := chain; len(bytes.TrimSpace(rest)) > 0; {
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(certs) >= 8 {
			return nil, errors.New("invalid CA chain")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
		rest = tail
	}
	if len(certs) < 2 {
		return nil, errors.New("online intermediate and offline root certificates required")
	}
	ca := certs[0]
	if ca.CheckSignatureFrom(ca) == nil {
		return nil, errors.New("online CA must not be a self-signed root")
	}
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 || ca.KeyUsage&x509.KeyUsageCRLSign == 0 || len(ca.SubjectKeyId) == 0 {
		return nil, errors.New("invalid issuing CA")
	}
	root := certs[len(certs)-1]
	if !root.IsCA || root.CheckSignatureFrom(root) != nil {
		return nil, errors.New("invalid root")
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range certs[1 : len(certs)-1] {
		intermediates.AddCert(c)
	}
	if _, err := ca.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, err
	}
	keyPEM, err := privateFile(keyFile)
	if err != nil {
		return nil, err
	}
	defer clear(keyPEM)
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid CA key")
	}
	var key any
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		err = errors.New("unsupported CA key")
	}
	clear(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("invalid signing key")
	}
	pub, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(pub, ca.RawSubjectPublicKeyInfo) {
		return nil, errors.New("CA key mismatch")
	}
	return &authority{ca, signer, chain, digest(ca.Raw)}, nil
}

func parseCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	key, ok := csr.PublicKey.(*rsa.PublicKey)
	if !ok || (key.N.BitLen() != 2048 && key.N.BitLen() != 3072 && key.N.BitLen() != 4096) || key.E != 65537 || csr.CheckSignature() != nil {
		return nil, errors.New("CSR key or signature rejected")
	}
	if csr.SignatureAlgorithm != x509.SHA256WithRSA && csr.SignatureAlgorithm != x509.SHA384WithRSA && csr.SignatureAlgorithm != x509.SHA512WithRSA && csr.SignatureAlgorithm != x509.SHA256WithRSAPSS && csr.SignatureAlgorithm != x509.SHA384WithRSAPSS && csr.SignatureAlgorithm != x509.SHA512WithRSAPSS {
		return nil, errors.New("CSR signature algorithm rejected")
	}
	if len(csr.Subject.Names) != 1 || !csr.Subject.Names[0].Type.Equal([]int{2, 5, 4, 3}) || !validName(csr.Subject.CommonName, 128) || len(csr.DNSNames) > 32 || len(csr.EmailAddresses)+len(csr.IPAddresses)+len(csr.URIs) != 0 {
		return nil, errors.New("CSR subject rejected")
	}
	for _, name := range csr.DNSNames {
		if !validName(name, 253) {
			return nil, errors.New("CSR DNS name rejected")
		}
	}
	for _, ext := range csr.Extensions {
		if !ext.Id.Equal([]int{2, 5, 29, 17}) {
			return nil, errors.New("CSR extension rejected")
		}
		var names []asn1.RawValue
		rest, err := asn1.Unmarshal(ext.Value, &names)
		if err != nil || len(rest) != 0 || len(names) != len(csr.DNSNames) {
			return nil, errors.New("CSR SAN rejected")
		}
		for _, name := range names {
			if name.Class != 2 || name.Tag != 2 || name.IsCompound {
				return nil, errors.New("CSR SAN type rejected")
			}
		}
	}
	return csr, nil
}

func validName(s string, max int) bool {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 || strings.ContainsRune("/\\\x00", c) {
			return false
		}
	}
	return true
}

func (a *authority) issue(csr *x509.CertificateRequest, serial *big.Int) ([]byte, error) {
	now := time.Now().UTC()
	if now.Before(a.cert.NotBefore) || !now.Add(24*time.Hour).Before(a.cert.NotAfter) {
		return nil, errors.New("CA validity insufficient")
	}
	leaf := &x509.Certificate{SerialNumber: serial, RawSubject: csr.RawSubject, DNSNames: csr.DNSNames, NotBefore: now, NotAfter: now.Add(24 * time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	issued, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	for rest := a.chain; len(bytes.TrimSpace(rest)) > 0; {
		block, tail := pem.Decode(rest)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(tail)) == 0 {
			roots.AddCert(c)
		} else {
			intermediates.AddCert(c)
		}
		rest = tail
	}
	if _, err := issued.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, statusError(422)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), a.chain...), nil
}
