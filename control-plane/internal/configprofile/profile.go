// Package configprofile defines the finite, node-local-TLS configuration identity.
package configprofile

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
	"github.com/google/uuid"
)

const Provider = "node-local-tls-v1"
const PlanCapability = "ocserv.config.complete.plan"
const ApplyCapability = "ocserv.config.complete.apply"

const canonicalDomain = "ocservia.complete-config.cbor.v1\x00"

var ErrInvalid = errors.New("invalid complete configuration profile")

var required = []string{"auth", "cookie-timeout", "device", "dns", "ipv4-network", "max-clients", "max-same-clients", "server-cert", "server-key", "socket-file", "tcp-port", "udp-port"}

type cborTLSRef struct {
	SecretRefID       []byte `cbor:"secret_ref_id"`
	Version           string `cbor:"version"`
	CertificateSHA256 []byte `cbor:"certificate_sha256"`
	SPKISHA256        []byte `cbor:"spki_sha256"`
	CASHA256          []byte `cbor:"ca_sha256"`
}

type cborDirective struct {
	Name    string      `cbor:"name"`
	Kind    uint32      `cbor:"kind"`
	Literal string      `cbor:"literal"`
	TLS     *cborTLSRef `cbor:"tls"`
}

type cborCandidate struct {
	NodeID           []byte          `cbor:"node_id"`
	ExpectedRevision uint64          `cbor:"expected_revision"`
	Directives       []cborDirective `cbor:"directives"`
}

func label(value string, max int) bool {
	if len(value) == 0 || len(value) > max || strings.Contains(value, "..") {
		return false
	}
	for i, b := range []byte(value) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			continue
		}
		if i > 0 && (b == '.' || b == '_' || b == '-') {
			continue
		}
		return false
	}
	return true
}

func validNetwork(value string) bool {
	prefix, err := netip.ParsePrefix(value)
	return err == nil && prefix.Addr().Is4() && prefix.Bits() >= 1 && prefix.Bits() <= 30 && prefix.Masked() == prefix && prefix.String() == value
}

func validLiteral(name, value string) bool {
	switch name {
	case "auth":
		return value == "plain[passwd=/etc/ocserv/ocpasswd]"
	case "socket-file":
		return value == "/run/ocserv.socket"
	case "device":
		return label(value, 15) && !strings.Contains(value, ".")
	case "ipv4-network":
		return validNetwork(value)
	case "route":
		return value == "default" || validNetwork(value)
	case "dns":
		address, err := netip.ParseAddr(value)
		return err == nil && address.Is4() && address.String() == value && !address.IsUnspecified() && !address.IsMulticast()
	case "tcp-port", "udp-port", "max-clients", "max-same-clients", "cookie-timeout":
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != value {
			return false
		}
		if name == "cookie-timeout" {
			return n >= 60 && n <= 86400
		}
		return n <= 65535 && (n > 0 || name == "udp-port")
	default:
		return false
	}
}

func nonNilBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte{}
	}
	return b
}

// Canonical is a versioned RFC 8949 Core Deterministic CBOR transcript, never
// protobuf serialization. It also rejects configurations outside the finite
// profile before they can be signed.
func Canonical(candidate *agentv1.CompleteConfigCandidate) ([]byte, error) {
	if candidate == nil || len(candidate.NodeId) != 16 || bytes.Equal(candidate.NodeId, make([]byte, 16)) || candidate.ExpectedRevision > uint64(^uint64(0)>>1) || len(candidate.Directives) < len(required) || len(candidate.Directives) > len(required)+2 {
		return nil, ErrInvalid
	}
	directives := make([]cborDirective, 0, len(candidate.Directives))
	seen := make(map[string]bool)
	previous := ""
	var serverRef *agentv1.NodeLocalTlsReference
	var maxClients, maxSame uint64
	for _, directive := range candidate.Directives {
		if directive == nil || directive.Name <= previous {
			return nil, ErrInvalid
		}
		previous = directive.Name
		seen[directive.Name] = true
		switch value := directive.Value.(type) {
		case *agentv1.CompleteConfigDirective_Literal:
			if !validLiteral(directive.Name, value.Literal) {
				return nil, ErrInvalid
			}
			directives = append(directives, cborDirective{
				Name:    directive.Name,
				Kind:    0,
				Literal: value.Literal,
				TLS:     nil,
			})
			if directive.Name == "max-clients" {
				maxClients, _ = strconv.ParseUint(value.Literal, 10, 32)
			}
			if directive.Name == "max-same-clients" {
				maxSame, _ = strconv.ParseUint(value.Literal, 10, 32)
			}
		case *agentv1.CompleteConfigDirective_Tls:
			ref := value.Tls
			if directive.Name != "server-cert" && directive.Name != "server-key" && directive.Name != "ca-cert" {
				return nil, ErrInvalid
			}
			if ref == nil || len(ref.SecretRefId) != 16 || bytes.Equal(ref.SecretRefId, make([]byte, 16)) || !label(ref.Version, 64) || len(ref.CertificateSha256) != 32 || len(ref.SpkiSha256) != 32 || (len(ref.CaSha256) != 0 && len(ref.CaSha256) != 32) || (directive.Name == "ca-cert" && len(ref.CaSha256) != 32) {
				return nil, ErrInvalid
			}
			if serverRef != nil && (!bytes.Equal(serverRef.SecretRefId, ref.SecretRefId) || serverRef.Version != ref.Version || !bytes.Equal(serverRef.CertificateSha256, ref.CertificateSha256) || !bytes.Equal(serverRef.SpkiSha256, ref.SpkiSha256) || !bytes.Equal(serverRef.CaSha256, ref.CaSha256)) {
				return nil, ErrInvalid
			}
			serverRef = ref
			directives = append(directives, cborDirective{
				Name: directive.Name,
				Kind: 1,
				TLS: &cborTLSRef{
					SecretRefID:       nonNilBytes(ref.SecretRefId),
					Version:           ref.Version,
					CertificateSHA256: nonNilBytes(ref.CertificateSha256),
					SPKISHA256:        nonNilBytes(ref.SpkiSha256),
					CASHA256:          nonNilBytes(ref.CaSha256),
				},
			})
		default:
			return nil, ErrInvalid
		}
	}
	for _, name := range required {
		if !seen[name] {
			return nil, ErrInvalid
		}
	}
	if maxSame > maxClients {
		return nil, ErrInvalid
	}
	return canonicalcbor.Marshal(canonicalDomain, cborCandidate{
		NodeID:           nonNilBytes(candidate.NodeId),
		ExpectedRevision: candidate.ExpectedRevision,
		Directives:       directives,
	})
}

func Hash(candidate *agentv1.CompleteConfigCandidate) ([32]byte, error) {
	canonical, err := Canonical(candidate)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func Redacted(candidate *agentv1.CompleteConfigCandidate) (string, error) {
	if _, err := Canonical(candidate); err != nil {
		return "", err
	}
	var result strings.Builder
	result.WriteString("# generated by ocservia complete-config/v1\n")
	for _, directive := range candidate.Directives {
		value := directive.GetLiteral()
		if ref := directive.GetTls(); ref != nil {
			id, _ := uuid.FromBytes(ref.SecretRefId)
			value = fmt.Sprintf("<secret-ref:%s:%s:%s>", id, Provider, ref.Version)
		}
		result.WriteString(directive.Name + " = " + value + "\n")
	}
	result.WriteString("run-as-user = ocservia-vpn\nrun-as-group = ocservia-vpn\nuse-occtl = true\n")
	return result.String(), nil
}
