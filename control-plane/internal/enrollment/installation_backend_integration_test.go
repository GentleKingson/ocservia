package enrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/commandauth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	enrollmentstore "github.com/GentleKingson/ocservia/control-plane/internal/enrollment/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation/attestationstore"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
)

var errAttestationRollback = errors.New("injected attestation registration rollback")

type attestationRollbackStore struct{ attestationstore.Store }

func (s attestationRollbackStore) BumpNodeRevision(ctx context.Context, node uuid.UUID, at time.Time) error {
	if err := s.Store.BumpNodeRevision(ctx, node, at); err != nil {
		return err
	}
	return errAttestationRollback
}

type installationRollbackBackend struct{ database.Backend }
type installationRollbackTx struct{ database.Tx }

func (b installationRollbackBackend) Begin(ctx context.Context, isolation database.Isolation) (database.Tx, error) {
	tx, err := b.Backend.Begin(ctx, isolation)
	if err != nil {
		return nil, err
	}
	return installationRollbackTx{tx}, nil
}
func (tx installationRollbackTx) EnrollmentStore() enrollmentstore.EnrollmentStore {
	store, _ := enrollmentstore.Enrollment(tx.Tx)
	return store
}
func (tx installationRollbackTx) PrivdAttestationStore() attestationstore.Store {
	store, _ := attestationstore.From(tx.Tx)
	return attestationRollbackStore{store}
}

func TestInstallationBackendIntegration(t *testing.T) {
	f, ctx := newLifecycleFixture(t), context.Background()
	signer, err := commandauth.NewRandomSigner()
	testutil.NoError(t, err)
	s := NewBackend(f.b, strings.Repeat("a", 64), "test", signer)
	testutil.NoError(t, s.ConfigureInstallation(InstallationTrust{Origin: "https://controller.example", RelayURL: "https://relay.example", RelayToken: strings.Repeat("r", 32)}))
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	makeToken := func() Token {
		t.Helper()
		token, err := s.CreateInstallationToken(ctx, BootstrapTokenSpec{WorkspaceID: f.workspace, Environment: "test", TTL: time.Minute, ActorID: "admin", Reason: "install", RequestID: uuid.NewString()})
		testutil.NoError(t, err)
		return token
	}
	token := makeToken()
	config, err := s.InstallationConfig(ctx, token.Value)
	testutil.NoError(t, err)
	if config.RelayAccessToken != strings.Repeat("r", 32) || config.CommandVerificationKeyHex != hex.EncodeToString(signer.PublicKey()) {
		t.Fatal("wrong installation trust")
	}
	if _, err := s.InstallationConfig(ctx, config.EnrollmentToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("child obtained root configuration", err)
	}
	endpoint := endpointFixture(109)
	request := enrollmentRequestCapabilities(config.EnrollmentToken, endpoint, []string{"ocserv.status.read", "ocserv.user.manage", privdattestation.AttestationCapability, ownersession.FencingCapability})
	response, err := s.Enroll(ctx, request)
	testutil.NoError(t, err)
	if response.Result != agentv1.HandshakeResult_HANDSHAKE_RESULT_ACCEPTED {
		t.Fatal("installation was not automatically activated")
	}
	node, err := uuid.FromBytes(response.NodeId)
	testutil.NoError(t, err)
	binding := InstallationBinding{Token: token.Value, NodeID: node, EndpointID: hex.EncodeToString(endpoint)}
	keyBinding := InstallationKeyBinding{InstallationBinding: binding, UserPasswordSealKeyID: request.SealingKeys[0].KeyId, UserPasswordSealPublicKeySHA256: hex.EncodeToString(request.SealingKeys[0].PublicKeySha256), P12PasswordSealKeyID: request.SealingKeys[1].KeyId, P12PasswordSealPublicKeySHA256: hex.EncodeToString(request.SealingKeys[1].PublicKeySha256)}
	challenge, err := s.InstallationChallenge(ctx, binding)
	if err != nil || challenge.Registered {
		t.Fatal("initial registration challenge", err)
	}
	f.within(t, func(tx database.Tx) error {
		store, err := enrollmentstore.Enrollment(tx)
		if err != nil {
			return err
		}
		caps, err := store.Capabilities(ctx, node, true)
		if err != nil {
			return err
		}
		for _, cap := range caps {
			if cap == privdattestation.AttestationCapability || cap == ownersession.FencingCapability {
				t.Fatal("root/fence capability granted during enrollment")
			}
		}
		return nil
	})
	makeRegistration := func() *agentv1.PrivdAttestationRegistrationV1 {
		t.Helper()
		public, key, err := ed25519.GenerateKey(rand.Reader)
		testutil.NoError(t, err)
		nonce, _ := hex.DecodeString(challenge.NonceHex)
		digest, _ := hex.DecodeString(challenge.ContextHex)
		r := &agentv1.PrivdAttestationRegistrationV1{Version: agentv1.PrivdReceiptVersion_PRIVD_RECEIPT_VERSION_V1, NodeId: node[:], PrivdAttestationKeyId: privdattestation.PublicKeyID(public), PublicKey: public, ControllerNonce: nonce, CredentialContextSha256: digest}
		canonical, err := privdattestation.CanonicalRegistrationV1(r)
		testutil.NoError(t, err)
		r.Signature = ed25519.Sign(key, canonical)
		return r
	}
	registration := makeRegistration()
	childBinding := keyBinding
	childBinding.Token = config.EnrollmentToken
	if _, err := s.RegisterInstallationKey(ctx, childBinding, registration, uuid.NewString()); err == nil {
		t.Fatal("Agent child credential registered a root key")
	}
	badBinding := binding
	badBinding.EndpointID = strings.Repeat("f", 64)
	if _, err := s.InstallationChallenge(ctx, badBinding); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("endpoint substitution accepted", err)
	}
	wrongSealing := keyBinding
	wrongSealing.UserPasswordSealPublicKeySHA256 = strings.Repeat("f", 64)
	if _, err := s.RegisterInstallationKey(ctx, wrongSealing, registration, uuid.NewString()); !errors.Is(err, ErrEndpointMismatch) {
		t.Fatal("root key registered with Agent-substituted sealing descriptor", err)
	}
	failing := *s
	failing.backend = installationRollbackBackend{f.b}
	if _, err := failing.RegisterInstallationKey(ctx, keyBinding, registration, uuid.NewString()); !errors.Is(err, errAttestationRollback) {
		t.Fatal("injected registration rollback", err)
	}
	if c, err := s.InstallationChallenge(ctx, binding); err != nil || c.Registered {
		t.Fatal("failed registration published a key", err)
	}
	// Concurrent identical requests recover one committed grant, never two keys.
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RegisterInstallationKey(ctx, keyBinding, registration, uuid.NewString())
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		testutil.NoError(t, err)
	}
	if _, err := s.RegisterInstallationKey(ctx, keyBinding, makeRegistration(), uuid.NewString()); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("bootstrap token authorized key replacement", err)
	}
	if again, err := s.Enroll(ctx, request); err != nil || string(again.NodeId) != string(response.NodeId) {
		t.Fatal("lost enrollment response recovery", err)
	}
	other := enrollmentRequestCapabilities(config.EnrollmentToken, endpointFixture(110), request.Capabilities)
	if _, err := s.Enroll(ctx, other); !errors.Is(err, ErrEndpointMismatch) {
		t.Fatal("consumed token rebound to another identity", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.InstallationConfig(ctx, token.Value); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired config disclosure", err)
	}
	if _, err := s.RegisterInstallationKey(ctx, keyBinding, registration, uuid.NewString()); err != nil {
		t.Fatal("committed registration cannot be recovered", err)
	}
	if c, err := s.InstallationChallenge(ctx, binding); err != nil || !c.Registered {
		t.Fatal("registered recovery status", err)
	}
	f.within(t, func(tx database.Tx) error {
		keys, err := attestationstore.From(tx)
		if err != nil {
			return err
		}
		active, err := keys.ActiveKeys(ctx, node, now)
		if len(active) != 1 {
			t.Fatal("duplicate keys", active)
		}
		return err
	})
	// An expired token must never authorize a first key, even after enrollment.
	now = now.Add(-2 * time.Minute)
	unregistered := makeToken()
	cfg, err := s.InstallationConfig(ctx, unregistered.Value)
	testutil.NoError(t, err)
	unregisteredEndpoint := endpointFixture(111)
	r, err := s.Enroll(ctx, enrollmentRequestCapabilities(cfg.EnrollmentToken, unregisteredEndpoint, request.Capabilities))
	testutil.NoError(t, err)
	id, _ := uuid.FromBytes(r.NodeId)
	now = now.Add(2 * time.Minute)
	if _, err := s.InstallationChallenge(ctx, InstallationBinding{Token: unregistered.Value, NodeID: id, EndpointID: hex.EncodeToString(unregisteredEndpoint)}); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired token granted first registration", err)
	}
	if verified, err := audit.NewBackendManager(f.b, nil).Verify(ctx, f.workspace); err != nil || !verified.Valid {
		t.Fatal("installation audit chain", err)
	}
}
