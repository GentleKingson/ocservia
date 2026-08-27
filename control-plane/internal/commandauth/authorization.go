// Package commandauth issues Controller command authorizations that cannot be
// minted by the transport process.
package commandauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// ProtocolVersion is the first command protocol revision that requires an
	// end-to-end Controller authorization proof.
	ProtocolVersion = "1.1"
	keyIDPrefix     = "ed25519-sha256:"
)

var authorizationV1Domain = []byte("ocservia/controller-command/v1\x00")
var sessionGrantV1Domain = []byte("ocservia/controller-session-grant/v1\x00")
var artifactGrantV1Domain = []byte("ocservia/artifact-grant/v1\x00")

// ClaimsV1 is the independent, non-Protobuf CommandAuthorizationV1 signing
// input. CanonicalV1 defines its exact cross-language byte representation.
type ClaimsV1 struct {
	AuthorizationVersion  uint32
	KeyID                 string
	ProtocolVersion       string
	CommandID             [16]byte
	IdempotencyKey        [16]byte
	NodeID                [16]byte
	OperationID           [16]byte
	ActorIdentity         string
	Action                string
	RequiredCapability    string
	ApprovalID            *[16]byte
	ApprovalRequestSHA256 *[32]byte
	ExpectedRevision      uint64
	SemanticHashVersion   uint32
	SemanticPayloadSHA256 [32]byte
	PayloadKind           uint32
	DeliveryMode          uint32
	IssuedAtSeconds       int64
	IssuedAtNanos         uint32
	ExpiresAtSeconds      int64
	ExpiresAtNanos        uint32
}

// Signer owns one active Ed25519 Controller command signing key.
type Signer struct {
	privateKey ed25519.PrivateKey
	keyID      string
}

// SessionGrantClaimsV1 is the independent canonical authorization for one
// mutation-capable Agent session.
type SessionGrantClaimsV1 struct {
	Version                uint32
	KeyID                  string
	ProtocolMajor          uint32
	ProtocolMinor          uint32
	NodeID                 [16]byte
	EndpointID             [32]byte
	AuthorizationRevision  uint64
	NegotiatedCapabilities []string
	IssuedAtSeconds        int64
	IssuedAtNanos          uint32
	ExpiresAtSeconds       int64
	ExpiresAtNanos         uint32
}

// ArtifactGrantClaimsV1 is the independent canonical authorization for one
// bounded P12 fetch. It is never derived from Protobuf serialization bytes.
type ArtifactGrantClaimsV1 struct {
	Version            uint32
	KeyID              string
	NodeID             [16]byte
	ArtifactID         [16]byte
	CertificateID      [16]byte
	CertificateVersion uint64
	OperationID        [16]byte
	AuthorizedSubject  string
	Purpose            string
	MaxBytes           uint64
	IssuedAtSeconds    int64
	IssuedAtNanos      uint32
	ExpiresAtSeconds   int64
	ExpiresAtNanos     uint32
	GrantID            [16]byte
}

// NewSignerFromSeed constructs a signer from an Ed25519 seed.
func NewSignerFromSeed(seed [ed25519.SeedSize]byte) *Signer {
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Signer{privateKey: append(ed25519.PrivateKey(nil), privateKey...), keyID: KeyID(publicKey)}
}

// NewRandomSigner creates an in-memory signer for development and tests.
func NewRandomSigner() (*Signer, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate command signing key: %w", err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Signer{privateKey: privateKey, keyID: KeyID(publicKey)}, nil
}

// LoadSigner loads a PKCS#8 PEM Ed25519 private key through a no-follow,
// descriptor-relative path walk with strict ownership, mode, and ancestry
// checks.
func LoadSigner(path string) (*Signer, error) {
	raw, err := readPrivateKeyFile(path, uint32(os.Geteuid()))
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("command signing key must contain exactly one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse command signing key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("command signing key must be Ed25519")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Signer{privateKey: append(ed25519.PrivateKey(nil), privateKey...), keyID: KeyID(publicKey)}, nil
}

// KeyID returns the stable identifier pinned alongside an Ed25519 public key.
func KeyID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return keyIDPrefix + hex.EncodeToString(digest[:])
}

// KeyID returns this signer's active key identifier.
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey returns a defensive copy of this signer's verification key.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.privateKey.Public().(ed25519.PublicKey)...)
}

// Authorize attaches a versioned Ed25519 proof after every command claim has
// reached its final value.
func (s *Signer) Authorize(envelope *agentv1.CommandEnvelope) error {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return errors.New("controller command signing key is unavailable")
	}
	envelope.Authorization = &agentv1.CommandAuthorizationProof{
		Version: agentv1.CommandAuthorizationVersion_COMMAND_AUTHORIZATION_VERSION_V1,
		KeyId:   s.keyID,
	}
	claims, err := ClaimsFromEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	canonical, err := CanonicalV1(claims)
	if err != nil {
		return err
	}
	envelope.Authorization.Signature = ed25519.Sign(s.privateKey, canonical)
	return nil
}

// IssueSessionGrant signs the exact Controller-authorized capability subset and
// trust revision for one Agent endpoint. Capabilities must already be sorted.
func (s *Signer) IssueSessionGrant(nodeID [16]byte, endpointID [32]byte, authorizationRevision uint64, negotiatedCapabilities []string, protocolMajor, protocolMinor uint32, issuedAt, expiresAt time.Time) (*agentv1.SessionGrantV1, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("controller command signing key is unavailable")
	}
	claims := SessionGrantClaimsV1{
		Version: uint32(agentv1.SessionGrantVersion_SESSION_GRANT_VERSION_V1),
		KeyID:   s.keyID, ProtocolMajor: protocolMajor, ProtocolMinor: protocolMinor,
		NodeID: nodeID, EndpointID: endpointID, AuthorizationRevision: authorizationRevision,
		NegotiatedCapabilities: append([]string(nil), negotiatedCapabilities...),
		IssuedAtSeconds:        issuedAt.Unix(), IssuedAtNanos: uint32(issuedAt.Nanosecond()),
		ExpiresAtSeconds: expiresAt.Unix(), ExpiresAtNanos: uint32(expiresAt.Nanosecond()),
	}
	canonical, err := CanonicalSessionGrantV1(claims)
	if err != nil {
		return nil, err
	}
	return &agentv1.SessionGrantV1{
		Version: agentv1.SessionGrantVersion_SESSION_GRANT_VERSION_V1,
		KeyId:   s.keyID, ProtocolMajor: protocolMajor, ProtocolMinor: protocolMinor,
		NodeId: nodeID[:], EndpointId: endpointID[:], AuthorizationRevision: authorizationRevision,
		NegotiatedCapabilities: append([]string(nil), negotiatedCapabilities...),
		IssuedAt:               timestamppb.New(issuedAt), ExpiresAt: timestamppb.New(expiresAt),
		Signature: ed25519.Sign(s.privateKey, canonical),
	}, nil
}

// IssueArtifactGrant authorizes exactly one bounded P12 artifact lease.
func (s *Signer) IssueArtifactGrant(nodeID, artifactID, certificateID [16]byte, certificateVersion uint64, operationID [16]byte, authorizedSubject string, maxBytes uint64, grantID [16]byte, issuedAt, expiresAt time.Time) (*agentv1.ArtifactGrantV1, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("controller command signing key is unavailable")
	}
	claims := ArtifactGrantClaimsV1{
		Version: uint32(agentv1.ArtifactGrantVersion_ARTIFACT_GRANT_VERSION_V1), KeyID: s.keyID,
		NodeID: nodeID, ArtifactID: artifactID, CertificateID: certificateID,
		CertificateVersion: certificateVersion, OperationID: operationID,
		AuthorizedSubject: authorizedSubject, Purpose: "certificate_p12", MaxBytes: maxBytes,
		IssuedAtSeconds: issuedAt.Unix(), IssuedAtNanos: uint32(issuedAt.Nanosecond()),
		ExpiresAtSeconds: expiresAt.Unix(), ExpiresAtNanos: uint32(expiresAt.Nanosecond()), GrantID: grantID,
	}
	canonical, err := CanonicalArtifactGrantV1(claims)
	if err != nil {
		return nil, err
	}
	return &agentv1.ArtifactGrantV1{
		Version: agentv1.ArtifactGrantVersion_ARTIFACT_GRANT_VERSION_V1, KeyId: s.keyID,
		NodeId: nodeID[:], ArtifactId: artifactID[:], CertificateId: certificateID[:],
		CertificateVersion: certificateVersion, OperationId: operationID[:], AuthorizedSubject: authorizedSubject,
		Purpose: "certificate_p12", MaxBytes: maxBytes, IssuedAt: timestamppb.New(issuedAt),
		ExpiresAt: timestamppb.New(expiresAt), GrantId: grantID[:], Signature: ed25519.Sign(s.privateKey, canonical),
	}, nil
}

// CanonicalArtifactGrantV1 returns the exact domain-separated artifact grant
// signing bytes using fixed-width big-endian integers and length-prefixed UTF-8.
func CanonicalArtifactGrantV1(claims ArtifactGrantClaimsV1) ([]byte, error) {
	if claims.Version != 1 || claims.KeyID == "" || len(claims.KeyID) > 128 ||
		claims.CertificateVersion == 0 || claims.AuthorizedSubject == "" || len(claims.AuthorizedSubject) > 256 ||
		claims.Purpose != "certificate_p12" || claims.MaxBytes == 0 || claims.MaxBytes > 64<<20 ||
		claims.IssuedAtNanos >= 1_000_000_000 || claims.ExpiresAtNanos >= 1_000_000_000 ||
		claims.ExpiresAtSeconds < claims.IssuedAtSeconds ||
		(claims.ExpiresAtSeconds == claims.IssuedAtSeconds && claims.ExpiresAtNanos <= claims.IssuedAtNanos) {
		return nil, errors.New("artifact grant v1 claims are invalid")
	}
	var encoded bytes.Buffer
	encoded.Grow(512)
	encoded.Write(artifactGrantV1Domain)
	writeUint32(&encoded, claims.Version)
	if err := writeString(&encoded, claims.KeyID); err != nil {
		return nil, err
	}
	encoded.Write(claims.NodeID[:])
	encoded.Write(claims.ArtifactID[:])
	encoded.Write(claims.CertificateID[:])
	writeUint64(&encoded, claims.CertificateVersion)
	encoded.Write(claims.OperationID[:])
	if err := writeString(&encoded, claims.AuthorizedSubject); err != nil {
		return nil, err
	}
	if err := writeString(&encoded, claims.Purpose); err != nil {
		return nil, err
	}
	writeUint64(&encoded, claims.MaxBytes)
	writeInt64(&encoded, claims.IssuedAtSeconds)
	writeUint32(&encoded, claims.IssuedAtNanos)
	writeInt64(&encoded, claims.ExpiresAtSeconds)
	writeUint32(&encoded, claims.ExpiresAtNanos)
	encoded.Write(claims.GrantID[:])
	return encoded.Bytes(), nil
}

// CanonicalSessionGrantV1 returns the exact domain-separated session grant
// signing bytes. It never serializes Protobuf.
func CanonicalSessionGrantV1(claims SessionGrantClaimsV1) ([]byte, error) {
	if claims.Version != 1 || claims.KeyID == "" || claims.ProtocolMajor == 0 || claims.AuthorizationRevision == 0 || claims.IssuedAtNanos >= 1_000_000_000 || claims.ExpiresAtNanos >= 1_000_000_000 || claims.ExpiresAtSeconds < claims.IssuedAtSeconds || (claims.ExpiresAtSeconds == claims.IssuedAtSeconds && claims.ExpiresAtNanos <= claims.IssuedAtNanos) {
		return nil, errors.New("session grant v1 claims are invalid")
	}
	if len(claims.NegotiatedCapabilities) > 128 || !slices.IsSorted(claims.NegotiatedCapabilities) {
		return nil, errors.New("session grant capabilities are invalid")
	}
	for index, capability := range claims.NegotiatedCapabilities {
		if capability == "" || len(capability) > 128 || (index > 0 && claims.NegotiatedCapabilities[index-1] == capability) {
			return nil, errors.New("session grant capabilities are invalid")
		}
	}
	var encoded bytes.Buffer
	encoded.Grow(512)
	encoded.Write(sessionGrantV1Domain)
	writeUint32(&encoded, claims.Version)
	if err := writeString(&encoded, claims.KeyID); err != nil {
		return nil, err
	}
	writeUint32(&encoded, claims.ProtocolMajor)
	writeUint32(&encoded, claims.ProtocolMinor)
	encoded.Write(claims.NodeID[:])
	encoded.Write(claims.EndpointID[:])
	writeUint64(&encoded, claims.AuthorizationRevision)
	writeUint32(&encoded, uint32(len(claims.NegotiatedCapabilities)))
	for _, capability := range claims.NegotiatedCapabilities {
		if err := writeString(&encoded, capability); err != nil {
			return nil, err
		}
	}
	writeInt64(&encoded, claims.IssuedAtSeconds)
	writeUint32(&encoded, claims.IssuedAtNanos)
	writeInt64(&encoded, claims.ExpiresAtSeconds)
	writeUint32(&encoded, claims.ExpiresAtNanos)
	return encoded.Bytes(), nil
}

// ClaimsFromEnvelopeV1 validates and projects a command envelope into the
// independent authorization claims model.
func ClaimsFromEnvelopeV1(envelope *agentv1.CommandEnvelope) (ClaimsV1, error) {
	if envelope == nil || envelope.GetAuthorization() == nil {
		return ClaimsV1{}, errors.New("command authorization proof is missing")
	}
	authorization := envelope.GetAuthorization()
	if authorization.GetVersion() != agentv1.CommandAuthorizationVersion_COMMAND_AUTHORIZATION_VERSION_V1 {
		return ClaimsV1{}, errors.New("command authorization version is unsupported")
	}
	if envelope.GetProtocolVersion() != ProtocolVersion {
		return ClaimsV1{}, errors.New("command protocol version does not require authorization v1")
	}
	payloadKind, expectedAction, expectedCapability, err := payloadAuthorization(envelope)
	if err != nil {
		return ClaimsV1{}, err
	}
	if envelope.GetAction() != expectedAction {
		return ClaimsV1{}, fmt.Errorf("command action %q does not match payload", envelope.GetAction())
	}
	if envelope.GetRequiredCapability() != expectedCapability {
		return ClaimsV1{}, fmt.Errorf("command capability %q does not match payload", envelope.GetRequiredCapability())
	}
	if strings.TrimSpace(envelope.GetActorId()) == "" || len(envelope.GetActorId()) > 256 || len(authorization.GetKeyId()) > 128 {
		return ClaimsV1{}, errors.New("command authorization identity is invalid")
	}
	commandID, err := fixed16(envelope.GetCommandId(), "command_id")
	if err != nil {
		return ClaimsV1{}, err
	}
	idempotencyKey, err := fixed16(envelope.GetIdempotencyKey(), "idempotency_key")
	if err != nil {
		return ClaimsV1{}, err
	}
	nodeID, err := fixed16(envelope.GetNodeId(), "node_id")
	if err != nil {
		return ClaimsV1{}, err
	}
	operationID, err := fixed16(envelope.GetOperationId(), "operation_id")
	if err != nil {
		return ClaimsV1{}, err
	}
	semanticHash, err := fixed32(envelope.GetSemanticPayloadSha256(), "semantic_payload_sha256")
	if err != nil {
		return ClaimsV1{}, err
	}
	if envelope.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V1 && envelope.GetSemanticPayloadHashVersion() != agentv1.SemanticPayloadHashVersion_SEMANTIC_PAYLOAD_HASH_VERSION_V2 {
		return ClaimsV1{}, errors.New("semantic payload hash version must be v1")
	}
	issuedAt, expiresAt := envelope.GetIssuedAt(), envelope.GetExpiresAt()
	if issuedAt == nil || issuedAt.CheckValid() != nil || expiresAt == nil || expiresAt.CheckValid() != nil || !expiresAt.AsTime().After(issuedAt.AsTime()) {
		return ClaimsV1{}, errors.New("command authorization timestamps are invalid")
	}
	if envelope.GetDeliveryMode() == agentv1.CommandDeliveryMode_COMMAND_DELIVERY_MODE_UNSPECIFIED {
		return ClaimsV1{}, errors.New("command delivery mode is invalid")
	}
	approvalID, err := optional16(envelope.GetApprovalId(), "approval_id")
	if err != nil {
		return ClaimsV1{}, err
	}
	approvalHash, err := optional32(envelope.GetApprovalRequestSha256(), "approval_request_sha256")
	if err != nil {
		return ClaimsV1{}, err
	}
	if approvalHash != nil && approvalID == nil {
		return ClaimsV1{}, errors.New("approval request hash requires an approval ID")
	}
	return ClaimsV1{
		AuthorizationVersion: uint32(authorization.GetVersion()),
		KeyID:                authorization.GetKeyId(), ProtocolVersion: envelope.GetProtocolVersion(),
		CommandID: commandID, IdempotencyKey: idempotencyKey, NodeID: nodeID, OperationID: operationID,
		ActorIdentity: envelope.GetActorId(), Action: envelope.GetAction(), RequiredCapability: envelope.GetRequiredCapability(),
		ApprovalID: approvalID, ApprovalRequestSHA256: approvalHash, ExpectedRevision: envelope.GetExpectedRevision(),
		SemanticHashVersion: uint32(envelope.GetSemanticPayloadHashVersion()), SemanticPayloadSHA256: semanticHash,
		PayloadKind: payloadKind, DeliveryMode: uint32(envelope.GetDeliveryMode()),
		IssuedAtSeconds: issuedAt.GetSeconds(), IssuedAtNanos: uint32(issuedAt.GetNanos()),
		ExpiresAtSeconds: expiresAt.GetSeconds(), ExpiresAtNanos: uint32(expiresAt.GetNanos()),
	}, nil
}

// CanonicalV1 returns the exact bytes signed by Controller Ed25519 keys. It
// never marshals a Protobuf message.
func CanonicalV1(claims ClaimsV1) ([]byte, error) {
	if claims.AuthorizationVersion != 1 || claims.KeyID == "" || claims.ProtocolVersion == "" || claims.ActorIdentity == "" || claims.Action == "" || claims.RequiredCapability == "" || claims.SemanticHashVersion == 0 || claims.PayloadKind == 0 || claims.DeliveryMode == 0 || claims.IssuedAtNanos >= 1_000_000_000 || claims.ExpiresAtNanos >= 1_000_000_000 || claims.ExpiresAtSeconds < claims.IssuedAtSeconds || (claims.ExpiresAtSeconds == claims.IssuedAtSeconds && claims.ExpiresAtNanos <= claims.IssuedAtNanos) {
		return nil, errors.New("command authorization v1 claims are invalid")
	}
	if claims.ApprovalRequestSHA256 != nil && claims.ApprovalID == nil {
		return nil, errors.New("approval request hash requires an approval ID")
	}
	var encoded bytes.Buffer
	encoded.Grow(512)
	encoded.Write(authorizationV1Domain)
	writeUint32(&encoded, claims.AuthorizationVersion)
	if err := writeString(&encoded, claims.KeyID); err != nil {
		return nil, err
	}
	if err := writeString(&encoded, claims.ProtocolVersion); err != nil {
		return nil, err
	}
	encoded.Write(claims.CommandID[:])
	encoded.Write(claims.IdempotencyKey[:])
	encoded.Write(claims.NodeID[:])
	encoded.Write(claims.OperationID[:])
	for _, value := range []string{claims.ActorIdentity, claims.Action, claims.RequiredCapability} {
		if err := writeString(&encoded, value); err != nil {
			return nil, err
		}
	}
	writeOptional16(&encoded, claims.ApprovalID)
	writeOptional32(&encoded, claims.ApprovalRequestSHA256)
	writeUint64(&encoded, claims.ExpectedRevision)
	writeUint32(&encoded, claims.SemanticHashVersion)
	encoded.Write(claims.SemanticPayloadSHA256[:])
	writeUint32(&encoded, claims.PayloadKind)
	writeUint32(&encoded, claims.DeliveryMode)
	writeInt64(&encoded, claims.IssuedAtSeconds)
	writeUint32(&encoded, claims.IssuedAtNanos)
	writeInt64(&encoded, claims.ExpiresAtSeconds)
	writeUint32(&encoded, claims.ExpiresAtNanos)
	return encoded.Bytes(), nil
}

func payloadAuthorization(envelope *agentv1.CommandEnvelope) (uint32, string, string, error) {
	switch envelope.GetPayload().(type) {
	case *agentv1.CommandEnvelope_SessionDisconnect:
		return 100, "session.disconnect", "ocserv.session.disconnect", nil
	case *agentv1.CommandEnvelope_UserCreate:
		return 101, "user.create", "ocserv.users.write", nil
	case *agentv1.CommandEnvelope_UserDisable:
		return 102, "user.disable", "ocserv.users.write", nil
	case *agentv1.CommandEnvelope_ConfigPlan:
		return 103, "config.plan", "ocserv.config.plan", nil
	case *agentv1.CommandEnvelope_ConfigApply:
		return 104, "config.apply", "ocserv.config.apply", nil
	case *agentv1.CommandEnvelope_ServiceReload:
		return 105, "service.reload", "ocserv.service.reload", nil
	case *agentv1.CommandEnvelope_SyntheticNoop:
		return 107, "operation.create", "synthetic.noop", nil
	case *agentv1.CommandEnvelope_SyntheticEcho:
		return 108, "operation.create", "synthetic.echo", nil
	case *agentv1.CommandEnvelope_SessionTerminate:
		return 112, "session.terminate", "ocserv.session.terminate", nil
	case *agentv1.CommandEnvelope_IpBanRemove:
		return 113, "ip_ban.remove", "ocserv.ip_ban.remove", nil
	case *agentv1.CommandEnvelope_UserPasswordRotate:
		return 114, "user.password.rotate", "ocserv.users.write", nil
	case *agentv1.CommandEnvelope_GroupApply:
		return 115, "group.apply", "ocserv.groups.write", nil
	case *agentv1.CommandEnvelope_UserEnable:
		return 116, "user.enable", "ocserv.users.write", nil
	case *agentv1.CommandEnvelope_CertificateCsr:
		return 117, "certificate.issue", "ocserv.certificate.issue", nil
	case *agentv1.CommandEnvelope_CertificateP12:
		return 118, "certificate.private_key.export", "ocserv.certificate.issue", nil
	case *agentv1.CommandEnvelope_CertificateRevoke:
		return 119, "certificate.revoke", "ocserv.certificate.revoke", nil
	case *agentv1.CommandEnvelope_AgentUpgrade:
		return 128, "agent.upgrade", "ocserv.agent.upgrade.v1", nil
	default:
		return 0, "", "", errors.New("command payload does not support authorization v1")
	}
}

func fixed16(value []byte, name string) ([16]byte, error) {
	if len(value) != 16 {
		return [16]byte{}, fmt.Errorf("%s must contain exactly 16 bytes", name)
	}
	return [16]byte(value), nil
}

func fixed32(value []byte, name string) ([32]byte, error) {
	if len(value) != 32 {
		return [32]byte{}, fmt.Errorf("%s must contain exactly 32 bytes", name)
	}
	return [32]byte(value), nil
}

func optional16(value []byte, name string) (*[16]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	fixed, err := fixed16(value, name)
	return &fixed, err
}

func optional32(value []byte, name string) (*[32]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	fixed, err := fixed32(value, name)
	return &fixed, err
}

func writeString(target io.Writer, value string) error {
	if len(value) > math.MaxUint32 {
		return errors.New("command authorization string exceeds uint32 length")
	}
	writeUint32(target, uint32(len(value)))
	_, err := io.WriteString(target, value)
	return err
}

func writeOptional16(target io.Writer, value *[16]byte) {
	if value == nil {
		_, _ = target.Write([]byte{0})
		return
	}
	_, _ = target.Write([]byte{1})
	_, _ = target.Write(value[:])
}

func writeOptional32(target io.Writer, value *[32]byte) {
	if value == nil {
		_, _ = target.Write([]byte{0})
		return
	}
	_, _ = target.Write([]byte{1})
	_, _ = target.Write(value[:])
}

func writeUint32(target io.Writer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func writeUint64(target io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func writeInt64(target io.Writer, value int64) { writeUint64(target, uint64(value)) }

func readPrivateKeyFile(path string, expectedUID uint32) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("command signing key path must be absolute and clean")
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if len(components) < 1 || components[len(components)-1] == "" {
		return nil, errors.New("command signing key path is invalid")
	}
	directoryFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open command signing key root: %w", err)
	}
	defer func() { _ = unix.Close(directoryFD) }()
	if err := validateDirectoryFD(directoryFD, expectedUID); err != nil {
		return nil, err
	}
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("command signing key ancestry is invalid")
		}
		nextFD, openErr := unix.Openat(directoryFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, fmt.Errorf("open command signing key ancestor: %w", openErr)
		}
		if err := validateDirectoryFD(nextFD, expectedUID); err != nil {
			unix.Close(nextFD)
			return nil, err
		}
		unix.Close(directoryFD)
		directoryFD = nextFD
	}
	fileFD, err := unix.Openat(directoryFD, components[len(components)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open command signing key: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), path)
	if file == nil {
		unix.Close(fileFD)
		return nil, errors.New("open command signing key file descriptor")
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		return nil, fmt.Errorf("stat command signing key: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != expectedUID || stat.Nlink != 1 {
		return nil, errors.New("command signing key must be a process-owned regular file with one link")
	}
	permissions := stat.Mode & 0o777
	if permissions != 0o400 && permissions != 0o600 {
		return nil, errors.New("command signing key mode must be 0400 or 0600")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, fmt.Errorf("read command signing key: %w", err)
	}
	if len(raw) == 0 || len(raw) > 4096 {
		return nil, errors.New("command signing key must contain 1..4096 bytes")
	}
	return raw, nil
}

func validateDirectoryFD(fd int, expectedUID uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat command signing key ancestry: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || (stat.Uid != 0 && stat.Uid != expectedUID) || stat.Mode&0o022 != 0 {
		return errors.New("command signing key ancestry must be root- or process-owned and not group/world writable")
	}
	return nil
}
