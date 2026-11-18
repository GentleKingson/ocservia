package certificates

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"slices"
	"sort"
	"strings"
	"time"
)

func validateSignedCertificate(csrDER, chainPEM []byte, now time.Time) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil, ErrInvalid
	}
	if len(chainPEM) > 256*1024 {
		return nil, ErrInvalid
	}
	remaining := chainPEM
	chain := make([]*x509.Certificate, 0, 4)
	for len(bytes.TrimSpace(remaining)) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(chain) >= 8 {
			return nil, ErrInvalid
		}
		certificate, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return nil, ErrInvalid
		}
		chain = append(chain, certificate)
		remaining = rest
	}
	if len(chain) < 2 {
		return nil, ErrInvalid
	}
	leaf := chain[0]
	root := chain[len(chain)-1]
	if !root.IsCA || root.CheckSignature(root.SignatureAlgorithm, root.RawTBSCertificate, root.Signature) != nil {
		return nil, ErrInvalid
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, intermediate := range chain[1 : len(chain)-1] {
		if !intermediate.IsCA {
			return nil, ErrInvalid
		}
		intermediates.AddCert(intermediate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, ErrInvalid
	}
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || now.Before(leaf.NotBefore.Add(-5*time.Minute)) || !now.Before(leaf.NotAfter) || !bytes.Equal(csr.RawSubject, leaf.RawSubject) {
		return nil, ErrInvalid
	}
	if len(leaf.EmailAddresses) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 {
		return nil, ErrInvalid
	}
	csrNames, leafNames := slices.Clone(csr.DNSNames), slices.Clone(leaf.DNSNames)
	sort.Strings(csrNames)
	sort.Strings(leafNames)
	if !slices.Equal(csrNames, leafNames) {
		return nil, ErrInvalid
	}
	csrKey, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, ErrInvalid
	}
	leafKey, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !strings.EqualFold(hex.EncodeToString(csrKey), hex.EncodeToString(leafKey)) {
		return nil, ErrInvalid
	}
	return leaf, nil
}
