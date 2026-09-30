package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type publicKeyFixtureSource struct{ data []byte }

func (s *publicKeyFixtureSource) UserPasswordPublicKey(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return s.data, nil
}

func TestUserPasswordSealingKeyIntegration(t *testing.T) {
	f := newApplyHTTPFixture(t)
	plan := f.plan(true)
	node := plan.NodeID
	f.bind(f.requester, node, "UserManager")
	f.exec(`DELETE FROM role_bindings WHERE identity_id=$1 AND workspace_id=$2`, `DELETE FROM role_bindings WHERE identity_id=? AND workspace_id=?`, f.reader.principal.IdentityID, f.workspace)
	f.bind(f.reader, node, "Viewer")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	endpoint := sha256.Sum256(node[:])
	at, _ := value.FromTime(time.Now())
	f.exec(`INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at) VALUES($1,1,1,'browser-key',$2,$3)`, `INSERT INTO node_sealing_keys(node_id,purpose,version,key_id,public_key_sha256,created_at) VALUES(?,1,1,'browser-key',?,?)`, node, digest[:], at)
	f.exec(`INSERT INTO node_capabilities(node_id,capability,approved) VALUES($1,'ocserv.users.write',true)`, `INSERT INTO node_capabilities(node_id,capability,approved) VALUES(?,'ocserv.users.write',true)`, node)
	binding := enrollment.UserPasswordSealingKey{WorkspaceID: f.workspace, NodeID: node, EndpointID: hex.EncodeToString(endpoint[:]), Version: 1, Purpose: "user_password", KeyID: "browser-key", PublicKeySHA256: hex.EncodeToString(digest[:]), PublicKeyDER: der}
	source := &publicKeyFixtureSource{}
	set := func(b enrollment.UserPasswordSealingKey) { source.data, _ = json.Marshal(b) }
	set(binding)
	service := enrollment.NewBackend(f.b, "", "test", f.signer)
	service.EnableUserPasswordPublicKeys(source)
	f.s.EnableEnrollment(service, nil)
	f.s.EnableUserState(userstate.NewWithSignerBackend(f.b, f.signer))
	path := "/api/v1/nodes/" + node.String() + "/user-password-sealing-key"
	w := f.call("GET", path, "", "", f.requester.cookie, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public key: %d %s", w.Code, w.Body)
	}
	var got enrollment.UserPasswordSealingKey
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || !bytes.Equal(got.PublicKeyDER, der) || got.KeyID != binding.KeyID {
		t.Fatal("incorrect verified public key")
	}
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.reader.cookie, nil), 403, "forbidden")
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", nil, nil), 401, "unauthenticated")

	for _, change := range []func(*enrollment.UserPasswordSealingKey){
		func(b *enrollment.UserPasswordSealingKey) { b.NodeID = uuid.Must(uuid.NewV7()) },
		func(b *enrollment.UserPasswordSealingKey) { b.WorkspaceID = uuid.Must(uuid.NewV7()) },
		func(b *enrollment.UserPasswordSealingKey) { b.EndpointID = hex.EncodeToString(digest[:]) },
		func(b *enrollment.UserPasswordSealingKey) { b.Purpose = "certificate_p12_password" },
		func(b *enrollment.UserPasswordSealingKey) { b.KeyID = "replaced-key" },
		func(b *enrollment.UserPasswordSealingKey) { b.PublicKeySHA256 = hex.EncodeToString(endpoint[:]) },
		func(b *enrollment.UserPasswordSealingKey) {
			b.PublicKeyDER = []byte("not SPKI")
			d := sha256.Sum256(b.PublicKeyDER)
			b.PublicKeySHA256 = hex.EncodeToString(d[:])
		},
	} {
		b := binding
		change(&b)
		set(b)
		assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 409, "sealing-key-binding-mismatch")
	}
	foreignWorkspace, foreignNode := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f.exec(`INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES($1,'Foreign key fixture',$2,$3,$4)`, `INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES(?,'Foreign key fixture',?,?,?)`, foreignWorkspace, foreignWorkspace.String(), at, at)
	f.exec(`INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES($1,$2,'Foreign key node','active',$3,$4)`, `INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at) VALUES(?,?,'Foreign key node','active',?,?)`, foreignNode, foreignWorkspace, at, at)
	assertApplyHTTPProblem(t, f.call("GET", "/api/v1/nodes/"+foreignNode.String()+"/user-password-sealing-key", "", "", f.requester.cookie, nil), 403, "forbidden")
	set(binding)
	// The same envelope reaches the existing signed command path for creation
	// and rotation. Owner fixture completion models an already applied command.
	for index, password := range []string{"test browser account 密码 one", "test browser account 密码 two"} {
		sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, []byte(password), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"name": "browser-fixture", "expected_version": index, "reason": "browser sealed account fixture", "sealed_password": map[string]any{"version": 1, "purpose": "user_password", "key_id": binding.KeyID, "ciphertext": sealed}})
		writePath := "/api/v1/nodes/" + node.String() + "/users"
		if index == 1 {
			writePath += "/browser-fixture:rotate-password"
		}
		w := f.call("POST", writePath, string(body), uuid.NewString(), f.requester.cookie, nil)
		if w.Code != 202 {
			t.Fatalf("account mutation %d: %d %s", index, w.Code, w.Body)
		}
		var operation struct {
			CommandID string `json:"command_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &operation); err != nil {
			t.Fatal(err)
		}
		id := uuid.MustParse(operation.CommandID)
		var encoded []byte
		if err := f.row(`SELECT envelope FROM commands WHERE id=$1`, `SELECT envelope FROM commands WHERE id=?`, id).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var envelope agentv1.CommandEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		var secret *agentv1.SealedSecretV1
		if index == 0 {
			secret = envelope.GetUserCreate().GetSealedPasswordV1()
		} else {
			secret = envelope.GetUserPasswordRotate().GetSealedPasswordV1()
		}
		if secret == nil || secret.KeyId != binding.KeyID {
			t.Fatal("incorrect envelope binding")
		}
		plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, secret.Ciphertext, nil)
		if err != nil || string(plain) != password || bytes.Contains(encoded, []byte(password)) {
			t.Fatal("password envelope did not round trip")
		}
		clear(plain)
		f.exec(`UPDATE commands SET state='succeeded' WHERE id=$1`, `UPDATE commands SET state='succeeded' WHERE id=?`, id)
	}
	// Offline keeps enrolled identity; capability withdrawal and endpoint
	// revocation invalidate the public key even if Signer still has its binding.
	f.exec(`UPDATE nodes SET status='offline' WHERE id=$1`, `UPDATE nodes SET status='offline' WHERE id=?`, node)
	if w := f.call("GET", path, "", "", f.requester.cookie, nil); w.Code != 200 {
		t.Fatal("offline enrolled key unavailable", w.Code)
	}
	f.exec(`UPDATE node_capabilities SET approved=false WHERE node_id=$1 AND capability='ocserv.users.write'`, `UPDATE node_capabilities SET approved=false WHERE node_id=? AND capability='ocserv.users.write'`, node)
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 409, "sealing-key-binding-mismatch")
	f.exec(`UPDATE node_capabilities SET approved=true WHERE node_id=$1 AND capability='ocserv.users.write'`, `UPDATE node_capabilities SET approved=true WHERE node_id=? AND capability='ocserv.users.write'`, node)
	f.exec(`UPDATE node_endpoint_keys SET state='revoked',revoked_at=$1 WHERE node_id=$2`, `UPDATE node_endpoint_keys SET state='revoked',revoked_at=? WHERE node_id=?`, at, node)
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 409, "sealing-key-binding-mismatch")
	f.exec(`UPDATE node_endpoint_keys SET state='active',revoked_at=NULL WHERE node_id=$1`, `UPDATE node_endpoint_keys SET state='active',revoked_at=NULL WHERE node_id=?`, node)
	// A rotated current descriptor invalidates the older Signer binding.
	f.exec(`UPDATE node_sealing_keys SET key_id='new-key' WHERE node_id=$1 AND purpose=1`, `UPDATE node_sealing_keys SET key_id='new-key' WHERE node_id=? AND purpose=1`, node)
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 409, "sealing-key-binding-mismatch")
	service.EnableUserPasswordPublicKeys(nil)
	assertApplyHTTPProblem(t, f.call("GET", path, "", "", f.requester.cookie, nil), 503, "sealing-key-unavailable")
	t.Log(fmt.Sprintf("public key binding and existing create/rotate commands verified with restricted runtime %T", f.b))
}
