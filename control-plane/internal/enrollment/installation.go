package enrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/google/uuid"
)

const installEnrollmentPrefix = "obt2_"
const InstallTokenPrefix = "oit1_"

// InstallationTrust is provisioned at Controller startup, never accepted from an
// unauthenticated enrollment request. Only the root installer receives it.
type InstallationTrust struct {
	Origin     string
	RelayURL   string
	RelayToken string
}

func (s *Service) ConfigureInstallation(trust InstallationTrust) error {
	if !validHTTPSOrigin(trust.Origin) {
		return ErrInvalidRequest
	}
	relay, err := url.Parse(trust.RelayURL)
	if err != nil || relay.Scheme != "https" || relay.Hostname() == "" || relay.User != nil || relay.RawQuery != "" || relay.Fragment != "" || len(trust.RelayToken) < 32 || len(trust.RelayToken) > 512 || strings.ContainsAny(trust.RelayToken, " \r\n\t") || s.signer == nil {
		return ErrInvalidRequest
	}
	s.installation = trust
	return nil
}

func validHTTPSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func (s *Service) CreateInstallationToken(ctx context.Context, spec BootstrapTokenSpec) (Token, error) {
	if s.installation.Origin == "" || s.signer == nil {
		return Token{}, ErrInvalidRequest
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return Token{}, err
	}
	root := InstallTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(s.installation.Origin)) + "." + base64.RawURLEncoding.EncodeToString(raw)
	child, ok := installationChild(root, s.installation.Origin)
	if !ok {
		return Token{}, ErrInvalidRequest
	}
	token, err := s.createBootstrapToken(ctx, spec, child)
	if err != nil {
		return Token{}, err
	}
	token.Value = root
	return token, nil
}

// Derivation is one-way and domain separated: possession of the Agent's child
// token never authorizes registration of a root-controlled privd key.
func installationChild(root, origin string) (string, bool) {
	if !strings.HasPrefix(root, InstallTokenPrefix) || len(root) > 2048 {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(root, InstallTokenPrefix), ".")
	if len(parts) != 2 {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(decoded) != origin || !validHTTPSOrigin(origin) || base64.RawURLEncoding.EncodeToString(decoded) != parts[0] {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != parts[1] {
		return "", false
	}
	digest := sha256.Sum256([]byte("ocservia/install-enrollment/v1\x00" + root))
	return installEnrollmentPrefix + base64.RawURLEncoding.EncodeToString(digest[:]), true
}

func bootstrapResult(_ string) agentv1.HandshakeResult {
	return agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED
}

type InstallationConfig struct {
	ControllerEndpointID      string    `json:"controller_endpoint_id"`
	Environment               string    `json:"environment"`
	EnrollmentToken           string    `json:"enrollment_token"`
	RelayURL                  string    `json:"relay_url"`
	RelayAccessToken          string    `json:"relay_access_token"`
	CommandVerificationKeyHex string    `json:"command_verification_key_hex"`
	ExpiresAt                 time.Time `json:"expires_at"`
}

func (s *Service) InstallationConfig(ctx context.Context, root string) (InstallationConfig, error) {
	child, ok := installationChild(root, s.installation.Origin)
	if !ok || s.signer == nil {
		return InstallationConfig{}, ErrInvalidToken
	}
	digest, _ := bootstrapTokenDigest(child)
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return InstallationConfig{}, err
	}
	defer rollback(tx)
	token, err := store.TokenByHash(ctx, digest[:], false)
	if errors.Is(err, database.ErrNotFound) {
		return InstallationConfig{}, ErrInvalidToken
	}
	if err != nil {
		return InstallationConfig{}, err
	}
	if !tokenUnexpired(token.ExpiresAt, s.now()) {
		return InstallationConfig{}, ErrInvalidToken
	}
	if token.ConsumedNode != nil {
		node, err := store.NodeByID(ctx, *token.ConsumedNode, enrollmentstore.ForShare)
		if err != nil || !bootstrapNodeState(child, node) || !slices.Equal(node.Endpoint, token.Endpoint) {
			return InstallationConfig{}, ErrInvalidToken
		}
	}
	return InstallationConfig{ControllerEndpointID: s.controllerEndpointID, Environment: token.Environment, EnrollmentToken: child, RelayURL: s.installation.RelayURL, RelayAccessToken: s.installation.RelayToken, CommandVerificationKeyHex: hex.EncodeToString(s.signer.PublicKey()), ExpiresAt: token.ExpiresAt}, nil
}

type InstallationBinding struct {
	Token      string    `json:"token"`
	NodeID     uuid.UUID `json:"node_id"`
	EndpointID string    `json:"endpoint_id"`
}

type InstallationKeyBinding struct {
	InstallationBinding
	UserPasswordSealKeyID           string `json:"user_password_seal_key_id"`
	UserPasswordSealPublicKeySHA256 string `json:"user_password_seal_public_key_sha256"`
	P12PasswordSealKeyID            string `json:"p12_password_seal_key_id"`
	P12PasswordSealPublicKeySHA256  string `json:"p12_password_seal_public_key_sha256"`
}

type InstallationChallenge struct {
	NonceHex   string `json:"controller_nonce_hex"`
	ContextHex string `json:"credential_context_sha256_hex"`
	Registered bool   `json:"registered"`
}

func installationChallenge(root string, node uuid.UUID) InstallationChallenge {
	nonce := sha256.Sum256([]byte("ocservia/install-privd-nonce/v1\x00" + root))
	context := sha256.Sum256([]byte("ocservia/install-privd-binding/v1\x00" + root + "\x00" + node.String()))
	return InstallationChallenge{NonceHex: hex.EncodeToString(nonce[:]), ContextHex: hex.EncodeToString(context[:])}
}

func (s *Service) lockInstallation(ctx context.Context, store enrollmentstore.EnrollmentStore, request InstallationBinding) (enrollmentstore.Token, error) {
	child, ok := installationChild(request.Token, s.installation.Origin)
	endpoint, err := hex.DecodeString(request.EndpointID)
	if !ok || err != nil || len(endpoint) != 32 || request.NodeID.Version() != 7 {
		return enrollmentstore.Token{}, ErrInvalidToken
	}
	digest, _ := bootstrapTokenDigest(child)
	token, err := store.TokenByHash(ctx, digest[:], true)
	if errors.Is(err, database.ErrNotFound) {
		return enrollmentstore.Token{}, ErrInvalidToken
	}
	if err != nil {
		return enrollmentstore.Token{}, err
	}
	if token.ConsumedNode == nil || *token.ConsumedNode != request.NodeID || !slices.Equal(token.Endpoint, endpoint) {
		return enrollmentstore.Token{}, ErrInvalidToken
	}
	node, err := store.NodeByID(ctx, request.NodeID, enrollmentstore.ForUpdate)
	if err != nil || node.WorkspaceID != token.WorkspaceID || !bootstrapNodeState(child, node) || !slices.Equal(node.Endpoint, endpoint) {
		return enrollmentstore.Token{}, ErrInvalidToken
	}
	return token, nil
}

// A consumed root token may only recover the same completed binding. Expiry
// still prevents first registration; any key history prevents a second grant.
func (s *Service) InstallationChallenge(ctx context.Context, request InstallationBinding) (InstallationChallenge, error) {
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return InstallationChallenge{}, err
	}
	defer rollback(tx)
	token, err := s.lockInstallation(ctx, store, request)
	if err != nil {
		return InstallationChallenge{}, err
	}
	keys, err := privdattestation.StoreFrom(tx)
	if err != nil {
		return InstallationChallenge{}, err
	}
	_, key, err := keys.FirstKey(ctx, request.NodeID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return InstallationChallenge{}, err
	}
	registered := err == nil && key.State == "active" && (key.ValidUntil == nil || key.ValidUntil.After(s.now()))
	if !registered && !tokenUnexpired(token.ExpiresAt, s.now()) {
		return InstallationChallenge{}, ErrInvalidToken
	}
	challenge := installationChallenge(request.Token, request.NodeID)
	challenge.Registered = registered
	return challenge, nil
}

func (s *Service) RegisterInstallationKey(ctx context.Context, request InstallationKeyBinding, registration *agentv1.PrivdAttestationRegistrationV1, requestID string) (string, error) {
	canonical, err := privdattestation.CanonicalRegistrationV1(registration)
	if err != nil || registration == nil || len(registration.Signature) != ed25519.SignatureSize || !ed25519.Verify(registration.PublicKey, canonical, registration.Signature) || !slices.Equal(registration.NodeId, request.NodeID[:]) || requestID == "" {
		return "", ErrInvalidRequest
	}
	challenge := installationChallenge(request.Token, request.NodeID)
	if hex.EncodeToString(registration.ControllerNonce) != challenge.NonceHex || hex.EncodeToString(registration.CredentialContextSha256) != challenge.ContextHex {
		return "", ErrInvalidToken
	}
	tx, store, err := s.begin(ctx, database.ReadCommitted)
	if err != nil {
		return "", err
	}
	defer rollback(tx)
	token, err := s.lockInstallation(ctx, store, request.InstallationBinding)
	if err != nil {
		return "", err
	}
	userDigest, userErr := hex.DecodeString(request.UserPasswordSealPublicKeySHA256)
	p12Digest, p12Err := hex.DecodeString(request.P12PasswordSealPublicKeySHA256)
	if userErr != nil || p12Err != nil {
		return "", ErrInvalidRequest
	}
	rootKeys := []*agentv1.SealingKeyDescriptorV1{
		{Version: 1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD, KeyId: request.UserPasswordSealKeyID, PublicKeySha256: userDigest},
		{Version: 1, Purpose: agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD, KeyId: request.P12PasswordSealKeyID, PublicKeySha256: p12Digest},
	}
	match, err := sealingKeysMatch(ctx, store, request.NodeID, rootKeys)
	if err != nil {
		return "", err
	}
	if !match {
		return "", ErrEndpointMismatch
	}
	keys, err := privdattestation.StoreFrom(tx)
	if err != nil {
		return "", err
	}
	existing, key, err := keys.FirstKey(ctx, request.NodeID)
	if err == nil {
		if existing != registration.PrivdAttestationKeyId || !slices.Equal(key.PublicKey, registration.PublicKey) || key.State != "active" || key.ValidUntil != nil {
			return "", ErrInvalidToken
		}
		return existing, nil
	}
	if !errors.Is(err, database.ErrNotFound) {
		return "", err
	}
	if !tokenUnexpired(token.ExpiresAt, s.now()) {
		return "", ErrInvalidToken
	}
	now := s.now().UTC()
	if err := keys.InsertKey(ctx, privdattestation.Key{NodeID: request.NodeID, KeyID: registration.PrivdAttestationKeyId, PublicKey: registration.PublicKey, CreatedAt: now, ApprovedAt: now, ActivatedAt: now}); err != nil {
		return "", err
	}
	if err := keys.ApproveCapability(ctx, request.NodeID, privdattestation.AttestationCapability); err != nil {
		return "", err
	}
	if err := keys.BumpNodeRevision(ctx, request.NodeID, now); err != nil {
		return "", err
	}
	summary, _ := json.Marshal(map[string]string{"key_id": registration.PrivdAttestationKeyId, "bootstrap_token_id": token.ID.String()})
	if err := audit.AppendChainTx(ctx, tx, audit.ChainRecord{WorkspaceID: token.WorkspaceID, ActorType: "bootstrap", ActorID: token.ID.String(), Action: "privd.attestation.key.register", ResourceType: "node", ResourceID: request.NodeID, NodeID: &request.NodeID, RequestID: requestID, Result: "succeeded", Reason: "root-only installation token", AfterSummary: summary, At: now}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return registration.PrivdAttestationKeyId, nil
}
