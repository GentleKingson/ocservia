// Package configprofile defines the finite, node-local-TLS configuration identity.
package configprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const Provider = "node-local-tls-v1"
const PlanCapability = "ocserv.config.complete.plan"
const ApplyCapability = "ocserv.config.complete.apply"

var ErrInvalid = errors.New("invalid complete configuration profile")

var required = []string{"auth", "cookie-timeout", "device", "dns", "ipv4-network", "max-clients", "max-same-clients", "server-cert", "server-key", "socket-file", "tcp-port", "udp-port"}

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

func appendLength(out []byte, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

// Canonical is a versioned transcript, never protobuf serialization. It also
// rejects configurations outside the finite profile before they can be signed.
func Canonical(candidate *agentv1.CompleteConfigCandidate) ([]byte, error) {
	if candidate == nil || len(candidate.NodeId) != 16 || bytes.Equal(candidate.NodeId, make([]byte, 16)) || candidate.ExpectedRevision > uint64(^uint64(0)>>1) || len(candidate.Directives) < len(required) || len(candidate.Directives) > len(required)+2 {
		return nil, ErrInvalid
	}
	out := append([]byte("ocservia.complete-config.v1\x00"), candidate.NodeId...)
	out = binary.BigEndian.AppendUint64(out, candidate.ExpectedRevision)
	out = binary.BigEndian.AppendUint32(out, uint32(len(candidate.Directives)))
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
		out = appendLength(out, []byte(directive.Name))
		switch value := directive.Value.(type) {
		case *agentv1.CompleteConfigDirective_Literal:
			if !validLiteral(directive.Name, value.Literal) {
				return nil, ErrInvalid
			}
			out = append(out, 0)
			out = appendLength(out, []byte(value.Literal))
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
			out = append(out, 1)
			out = append(out, ref.SecretRefId...)
			out = appendLength(out, []byte(ref.Version))
			out = append(out, ref.CertificateSha256...)
			out = append(out, ref.SpkiSha256...)
			out = appendLength(out, ref.CaSha256)
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
	return out, nil
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

func ApprovalHash(planID, nodeID uuid.UUID, candidate, materialized, current []byte, revision uint64, expires *timestamppb.Timestamp) ([]byte, error) {
	if planID == uuid.Nil || nodeID == uuid.Nil || len(candidate) != 32 || len(materialized) != 32 || len(current) != 32 || expires == nil || !expires.IsValid() {
		return nil, ErrInvalid
	}
	out := append([]byte("ocservia.complete-config.approval.v1\x00"), planID[:]...)
	out = append(out, nodeID[:]...)
	out = append(out, candidate...)
	out = append(out, materialized...)
	out = append(out, current...)
	out = binary.BigEndian.AppendUint64(out, revision)
	out = binary.BigEndian.AppendUint64(out, uint64(expires.Seconds))
	out = binary.BigEndian.AppendUint32(out, uint32(expires.Nanos))
	hash := sha256.Sum256(out)
	return hash[:], nil
}
