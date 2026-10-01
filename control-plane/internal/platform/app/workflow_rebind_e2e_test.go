package app

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A second real Controller, with its own database, signing key and transport identity.
func startRebindTarget(t *testing.T, source *controllerE2E) (*controllerE2E, string) {
	f := newControllerE2EAt(t, source.ctx, "/run/pr02-rebind-target")
	f.artifacts = filepath.Join(source.artifacts, "target")
	if err := os.MkdirAll(f.artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	ownerOptions, runtimeOptions, account := controllerProcessDatabase(t)
	if ownerOptions.Backend == "postgres" {
		ownerURL, _ := url.Parse(ownerOptions.URL)
		ownerURL.Path = "/ocservia_rebind_target"
		ownerOptions.URL = ownerURL.String()
		runtimeURL, _ := url.Parse(runtimeOptions.URL)
		runtimeURL.Path = "/ocservia_rebind_target"
		runtimeOptions.URL = runtimeURL.String()
	}
	f.run(0, 0, nil, "/usr/bin/cp", source.root+"/ocserv-control", f.root+"/ocserv-control")
	f.run(0, 0, f.environment(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}), f.root+"/ocserv-control", "--migrate-only")
	owner, err := connection.Open(f.ctx, ownerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	installSchedulerEvidence(t, f.ctx, owner, runtimeOptions.Backend, account)
	f.workspace = uuid.Must(uuid.NewV7()).String()
	workspace := uuid.MustParse(f.workspace)
	at, _ := value.FromTime(time.Now().UTC())
	if ownerOptions.Backend == "postgres" {
		_, err = owner.Store.Exec(f.ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES($1,'real E2E',$2,$3,$3)`, workspace, f.workspace, at)
	} else {
		_, err = owner.Store.Exec(f.ctx, `INSERT INTO workspaces(id,name,slug,created_at,updated_at)VALUES(?,'real E2E',?,?,?)`, mysql.UUIDBytes(workspace), f.workspace, at, at)
	}
	if err != nil {
		t.Fatal("initialize empty test workspace", err)
	}
	adminPassword, approverPassword := "isolated controller E2E administrator password", "independent controller E2E approver password"
	f.file("admin-password", []byte(adminPassword), 0, 0, 0600)
	f.file("approver-password", []byte(approverPassword), 0, 0, 0600)
	f.run(0, 0, f.environment(runtimeOptions, map[string]string{
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME": "e2e-admin", "OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE": f.root + "/admin-password",
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID": f.workspace, "OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME": "e2e-approver",
		"OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE": f.root + "/approver-password",
	}), f.root+"/ocserv-control", "--bootstrap-local-admin")

	controllerPublic, controllerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, _ := x509.MarshalPKCS8PrivateKey(controllerPrivate)
	publicDER, _ := x509.MarshalPKIXPublicKey(controllerPublic)
	f.file("controller/signing.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 65534, 65532, 0600)
	f.file("transport-verification.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0, 65532, 0640)
	for name, ids := range map[string][2]int{"agent": {65533, 65533}, "privd": {0, 65533}} {
		f.file(name+"/verification.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), ids[0], ids[1], 0600)
	}
	endpointPublic, endpointPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpointID := hex.EncodeToString(endpointPublic)
	f.file("transport/identity", endpointPrivate.Seed(), 65532, 65532, 0600)
	f.relays = source.relays
	for _, name := range []string{"relay-ca.pem", "transport/relay-token"} {
		data, err := os.ReadFile(source.root + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		f.file(name, data, 65532, 65532, 0600)
	}
	signer := f.startSigner()
	address := e2eAddress(t)
	f.base = "http://" + address
	controllerEnvironment := f.environment(runtimeOptions, map[string]string{
		"OCSERV_HTTP_ADDRESS": address, "OCSERV_PUBLIC_ORIGIN": f.base,
		"OCSERV_CONTROLLER_ENDPOINT_ID": endpointID, "OCSERV_COMMAND_SIGNING_KEY_FILE": f.root + "/controller/signing.pem",
		"OCSERV_TRANSPORT_SOCKET": f.root + "/transport/control.sock", "OCSERV_TRUST_SOCKET": f.root + "/controller/trust.sock",
		"OCSERV_TRANSPORT_UID": "65532", "OCSERV_TRANSPORT_GID": "65532",
		"OCSERV_CERTIFICATE_SIGNER_URL": signer.URL, "OCSERV_CERTIFICATE_SIGNER_TOKEN": "isolated-test-signer",
		"SSL_CERT_FILE": f.root + "/signer-ca.pem",
		"OCSERV_TEST_SCHEDULER_MAINTENANCE_EVIDENCE": "true",
	})
	roles := []string{"all"}
	switch os.Getenv("PR07_CONTROLLER_ROLE_MODE") {
	case "", "all":
	case "split":
		roles = []string{"api", "worker", "scheduler"}
	default:
		t.Fatal("invalid Controller E2E role mode")
	}
	for _, role := range roles {
		f.start("controller-"+role, 65534, 65532, controllerEnvironment, f.root+"/ocserv-control", "--role="+role)
	}
	f.wait("scheduler maintenance commit", func() bool {
		var completed int
		return owner.Store.QueryRow(f.ctx, `SELECT count(*) FROM g6_scheduler_maintenance_history`).Scan(&completed) == nil && completed > 0
	})
	f.wait("Controller trust socket", func() bool { _, err := os.Stat(f.root + "/controller/trust.sock"); return err == nil })
	// Match deployed enrollment policy: new nodes have no owner session yet.
	// Once the Agent negotiates fencing, transportd always requires its binding.
	transportArgs := []string{"--socket", f.root + "/transport/control.sock", "--key-file", f.root + "/transport/identity", "--trust-socket", f.root + "/controller/trust.sock", "--control-plane-uid", "65534", "--control-plane-gid", "65532", "--controller-verification-key-file", f.root + "/transport-verification.pem"}
	transportArgs = append(transportArgs, f.relayArgs("transport")...)
	f.start("transport", 65532, 65532, nil, "/usr/local/bin/ocservia-transportd", transportArgs...)
	f.wait("real transport service socket", func() bool { _, err := os.Stat(f.root + "/transport/control.sock"); return err == nil })
	f.wait("Controller readiness", func() bool {
		response, err := f.client.Get(f.base + "/readyz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	f.admin = f.login("e2e-admin", adminPassword)
	f.approver = f.login("e2e-approver", approverPassword)

	return f, endpointID
}

func (f *controllerE2E) rebindWorkflow(oldNode, endpoint, oldController string, sealArgs, privdArgs, capabilities []string, disposition string, sourceOptions connection.Options) {
	t := f.t
	if disposition != "revoked" && disposition != "unreachable" {
		t.Fatal("invalid rebind disposition")
	}
	target, targetEndpoint := startRebindTarget(t, f)
	if err := os.Chmod("/var/lib/ocservia-upgrade", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown("/var/lib/ocservia-upgrade", 0, 0); err != nil {
		t.Fatal(err)
	}
	copyFile := func(source, destination string, uid, gid int, mode os.FileMode) {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(destination, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	for _, binary := range []string{"ocservia-agent", "ocservia-privd", "ocservia-upgrader"} {
		copyFile("/usr/local/bin/"+binary, "/usr/libexec/ocservia/"+binary, 0, 0, 0755)
	}
	for source, dest := range map[string]string{"scripts/rebind-agent.py": "ocservia-agent-rebind", "scripts/retain-agent.py": "ocservia-agent-retention", "deploy/production/systemd/agent-relays.sh": "ocservia-agent-relays"} {
		copyFile("/workspace/"+source, "/usr/libexec/ocservia/"+dest, 0, 0, 0755)
	}
	copyFile(f.root+"/transport-verification.pem", "/etc/ocservia-agent/command.pem", 0, 65533, 0640)
	copyFile(target.root+"/transport-verification.pem", "/etc/ocservia-agent/target.pem", 0, 65533, 0640)
	copyFile(f.root+"/agent/relay-token", "/etc/ocservia-agent/relay-access-token", 0, 65533, 0640)
	copyFile(f.root+"/relay-ca.pem", "/etc/ocservia-agent/relay-ca.pem", 0, 0, 0444)
	env := map[string]string{"PATH": "/usr/bin:/bin", "CONTROLLER_ENDPOINT_ID": oldController, "NODE_ID": oldNode,
		"AGENT_ENDPOINT_ID": endpoint, "CONTROLLER_COMMAND_VERIFICATION_KEY_FILE": "/etc/ocservia-agent/command.pem",
		"RELAY_URL_A": f.relays[0], "RELAY_URL_B": "", "RUST_LOG": "info"}
	for i := 0; i < len(sealArgs); i += 2 {
		env[strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(sealArgs[i], "--"), "-", "_"))] = sealArgs[i+1]
	}
	var agentEnv strings.Builder
	for k, v := range env {
		if k != "PATH" && k != "RUST_LOG" && !strings.HasPrefix(k, "RELAY_") {
			fmt.Fprintf(&agentEnv, "%s=%s\n", k, v)
		}
	}
	writeProtected := func(path string, content []byte) {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProtected("/etc/ocservia-agent/agent.env", []byte(agentEnv.String()))
	writeProtected("/etc/ocservia-agent/relays.env", []byte(fmt.Sprintf("RELAY_URL_A=%s\nRELAY_URL_B=\n", f.relays[0])))
	// The container supervisor starts the packaged relay launcher and actual privd.
	// Its only substitution is PID supervision in a container without systemd PID 1.
	if err := os.MkdirAll("/run/rebind-services", 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.processes {
		if p.name == "agent" || p.name == "privd" {
			argv := append([]string{"/usr/libexec/ocservia/ocservia-privd"}, privdArgs...)
			uid := 0
			if p.name == "agent" {
				argv = []string{"/usr/libexec/ocservia/ocservia-agent-relays"}
				uid = 65533
			}
			data, _ := json.Marshal(map[string]any{"pid": p.cmd.Process.Pid, "argv": argv, "env": env, "uid": uid})
			writeProtected("/run/rebind-services/ocservia-"+p.name+".service.json", data)
		}
	}
	copyFile("/usr/bin/systemctl", "/usr/bin/ocservia-e2e-ocserv-systemctl", 0, 0, 0755)
	copyFile("/workspace/deploy/database-e2e/rebind-systemctl.py", "/usr/bin/systemctl", 0, 0, 0755)
	t.Cleanup(func() {
		_ = f.command(0, 0, nil, "/usr/bin/systemctl", "stop", "ocservia-agent.service", "ocservia-privd.service").Run()
		for _, name := range []string{"ocservia-agent.service.log", "ocservia-privd.service.log"} {
			if data, err := os.ReadFile("/run/rebind-services/" + name); err == nil {
				_ = os.WriteFile(filepath.Join(f.artifacts, name), data, 0600)
			}
		}
	})
	// Preserve bytes, not just path existence, across the authority transaction.
	preserved := map[string][]byte{}
	for _, path := range []string{"/etc/ocserv/ocserv.conf", "/etc/ocserv/ocpasswd", f.root + "/relay.pem", f.root + "/relay.key", f.root + "/privd/user.key", f.root + "/privd/p12.key", f.root + "/privd/state/attestation.key", "/var/lib/ocservia-agent/identity/endpoint.key"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		preserved[path] = data
	}
	var returning []*e2eProcess
	if disposition == "unreachable" {
		for _, p := range f.processes {
			if p.name == "transport" || p.name == "controller-all" {
				returning = append(returning, p)
			}
		}
	}
	if disposition == "revoked" {
		approval := f.approve("node.revoke", "node", oldNode, nil)
		f.api(f.admin, "POST", "/api/v1/nodes/"+oldNode+"/revocation", map[string]any{"reason": "explicit Controller replacement"}, map[string]string{"X-Approval-ID": approval}, 200, 503)
	} else {
		f.stop("transport")
		f.stop("controller-all")
	}
	token := target.api(target.admin, "POST", "/api/v1/node-bootstrap-tokens", map[string]any{"workspace_id": target.workspace, "environment": "development", "expected_node_name": "rebound-node", "expected_endpoint_id": endpoint, "ttl_seconds": 600, "reason": "same EndpointID, independent authority"}, nil, 201)
	writeProtected("/etc/ocservia-agent/target.token", []byte(e2eString(t, token, "token")))
	cli := "/usr/libexec/ocservia/ocservia-agent-rebind"
	output := f.run(0, 0, nil, cli, "prepare", "--controller", targetEndpoint, "--command-key-file", "/etc/ocservia-agent/target.pem", "--token-file", "/etc/ocservia-agent/target.token", "--environment", "development", "--source-disposition", disposition, "--reason", "real Controller transfer")
	var operation, statePath string
	entries, err := os.ReadDir("/var/lib/ocservia-rebind")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			operation = entry.Name()
			statePath = "/var/lib/ocservia-rebind/" + operation + "/state.json"
		}
	}
	readState := func() map[string]any {
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err = json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	state := readState()
	newNode := e2eString(t, state, "node")
	if newNode == oldNode || !bytes.Contains(output, []byte("ENROLLED_LOCAL")) {
		t.Fatal("new Controller did not allocate independent pending node", string(output))
	}
	path := "/api/v1/nodes/" + newNode
	trust := map[string]any{"labels": map[string]string{"fixture": "real-rebind"}, "policy": "default", "capabilities": capabilities}
	approval := target.approve("node.approve", "node", newNode, map[string]any{"node_approval": trust})
	trust["reason"] = "approve rebound node independently"
	target.api(target.admin, "POST", path+"/approval", trust, map[string]string{"X-Approval-ID": approval}, 200, 503)
	credential := target.api(target.admin, "POST", path+"/privd-attestation-credentials", map[string]any{"ttl_seconds": 300, "reason": "register preserved root key"}, nil, 201)
	registration := f.run(0, 65533, nil, "/usr/local/bin/ocservia-privd", "attestation-registration", f.root+"/privd/state/attestation.key", newNode, e2eString(t, credential, "controller_nonce_hex"), e2eString(t, credential, "credential_context_sha256_hex"))
	var body map[string]any
	if err = json.Unmarshal(registration, &body); err != nil {
		t.Fatal(err)
	}
	body["credential"] = e2eString(t, credential, "credential")
	target.api(nil, "POST", path+"/privd-attestation-keys:register", body, nil, 201)
	output = f.run(0, 0, nil, cli, "commit", operation)
	state = readState()
	if state["phase"] != "verified" || state["mutations_blocked"] != false {
		t.Fatal("binding not verified and clear", state, string(output))
	}
	for _, name := range []string{"agent", "privd"} {
		for i, p := range f.processes {
			if p.name == name {
				<-p.done
				f.processes = append(f.processes[:i], f.processes[i+1:]...)
				break
			}
		}
	}
	for path, before := range preserved {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("business/key state changed", path, err)
		}
	}
	key, err := os.ReadFile("/var/lib/ocservia-agent/bindings/" + newNode + "/identity/endpoint.key")
	if err != nil || !bytes.Equal(key, preserved["/var/lib/ocservia-agent/identity/endpoint.key"]) {
		t.Fatal("Endpoint private key changed", err)
	}
	target.wait("new authority session", func() bool {
		return target.api(target.admin, "GET", path, nil, nil, 200)["connection_state"] == "online"
	})

	for _, p := range returning {
		uid := 65534
		if p.name == "transport" {
			uid = 65532
		}
		f.start(p.name, uid, 65532, p.cmd.Env, p.cmd.Args[6], p.cmd.Args[7:]...)
		if p.name == "controller-all" {
			f.wait("returning Controller trust socket", func() bool { _, err := os.Stat(f.root + "/controller/trust.sock"); return err == nil })
		}
	}
	f.wait("old Controller restored without Agent session", func() bool {
		response, err := f.client.Get(f.base + "/readyz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return false
		}
		node := f.api(f.admin, "GET", "/api/v1/nodes/"+oldNode, nil, nil, 200)
		return node["connection_state"] != "online"
	})
	if disposition == "revoked" {
		node := f.api(f.admin, "GET", "/api/v1/nodes/"+oldNode, nil, nil, 200)
		if node["trust_status"] != "revoked" {
			t.Fatal("source revocation not retained", node)
		}
	}
	f.rejectOldRootAuthority(sourceOptions, oldNode)
	// A new target command must reach the actual root helper and change Ocserv.
	sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &f.sealKeys["user_password"].PublicKey, []byte("rebound-user-password"), nil)
	if err != nil {
		t.Fatal(err)
	}
	op := target.api(target.admin, "POST", path+"/users", map[string]any{"name": "rebound-user", "expected_version": 0, "reason": "new authority mutation", "sealed_password": map[string]any{"version": 1, "purpose": "user_password", "key_id": "e2e-user_password", "ciphertext": sealed}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 202)
	target.waitOperation(e2eString(t, op, "id"))
	passwd, err := os.ReadFile("/etc/ocserv/ocpasswd")
	if err != nil || !bytes.Contains(passwd, []byte("rebound-user:")) || !bytes.Contains(passwd, []byte("real-e2e-user:")) {
		t.Fatal("new mutation or retained user missing", err)
	}
	journal := "/var/lib/ocservia-agent/bindings/" + newNode + "/agent.db"
	sessionBefore := f.run(0, 0, nil, "sqlite3", journal, "SELECT hex(value) FROM agent_metadata WHERE key='verified_session_grant';")
	f.run(0, 0, nil, "/usr/bin/systemctl", "stop", "ocservia-agent.service", "ocservia-privd.service")
	f.run(0, 0, nil, cli, "commit", operation) // verified commit does not restart; exercise services separately
	f.run(0, 0, nil, "/usr/bin/systemctl", "start", "ocservia-privd.service", "ocservia-agent.service")
	target.wait("fresh signed grant after process restart", func() bool {
		sessionAfter := f.run(0, 0, nil, "sqlite3", journal, "SELECT hex(value) FROM agent_metadata WHERE key='verified_session_grant';")
		return len(bytes.TrimSpace(sessionAfter)) > 0 && !bytes.Equal(sessionBefore, sessionAfter)
	})
	target.wait("restarted target session", func() bool {
		return target.api(target.admin, "GET", path, nil, nil, 200)["connection_state"] == "online"
	})
	// Original history is still present, and the independent worker refuses a recent epoch.
	before := f.run(0, 0, nil, "sqlite3", "/var/lib/ocservia-agent/agent.db", "SELECT count(*) FROM command_journal;")
	f.run(0, 0, nil, "/usr/libexec/ocservia/ocservia-agent-retention")
	after := f.run(0, 0, nil, "sqlite3", "/var/lib/ocservia-agent/agent.db", "SELECT count(*) FROM command_journal;")
	if !bytes.Equal(before, after) || strings.TrimSpace(string(before)) == "0" {
		t.Fatal("source command history lost")
	}

	// Age only the fixture's protected completion timestamp, then run the real
	// independent worker. Existing root receipts and replay identities survive.
	state = readState()
	state["verified_at"] = time.Now().Add(-400 * 24 * time.Hour).Unix()
	aged, _ := json.Marshal(state)
	writeProtected(statePath, aged)
	f.run(0, 0, nil, "/usr/libexec/ocservia/ocservia-agent-retention")
	compacted := readState()
	if compacted["source_state"] != "compacted" || compacted["details_compacted_at"] == nil {
		t.Fatal("eligible source epoch was not compacted", compacted)
	}
	if _, err := os.Stat(filepath.Dir(statePath) + "/token"); !os.IsNotExist(err) {
		t.Fatal("expired bootstrap detail survived", err)
	}
	retained := f.run(0, 0, nil, "sqlite3", "/var/lib/ocservia-agent/agent.db", "SELECT count(*) FROM command_journal;")
	if !bytes.Equal(before, retained) {
		t.Fatal("retention removed command identity/proof")
	}
	marker := f.run(0, 0, nil, "sqlite3", "/var/lib/ocservia-agent/agent.db", "SELECT count(*) FROM agent_metadata WHERE key='retired_binding';")
	if strings.TrimSpace(string(marker)) != "1" {
		t.Fatal("retired tombstone missing")
	}
	t.Logf("real rebind %s: EndpointID %s retained; NodeID %s -> %s; target mutation/restart accepted; source journal and business state preserved", disposition, endpoint, oldNode, newNode)
}

func (f *controllerE2E) createRebindSourceUser(nodePath string) {
	sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &f.sealKeys["user_password"].PublicKey, []byte("preserved-source-password"), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	op := f.api(f.admin, "POST", nodePath+"/users", map[string]any{"name": "real-e2e-user", "expected_version": 0, "reason": "preserved business state", "sealed_password": map[string]any{"version": 1, "purpose": "user_password", "key_id": "e2e-user_password", "ciphertext": sealed}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 202)
	f.waitOperation(e2eString(f.t, op, "id"))
	data, err := os.ReadFile("/etc/ocserv/ocpasswd")
	if err != nil || !bytes.Contains(data, []byte("real-e2e-user:")) {
		f.t.Fatal("source user not created", err)
	}
}

// Replay a real, still-unexpired Controller A envelope directly as the Agent
// principal. Even a compromised unprivileged Agent cannot recover A's authority.
func (f *controllerE2E) rejectOldRootAuthority(options connection.Options, node string) {
	t := f.t
	db, err := connection.Open(f.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := "SELECT envelope FROM commands WHERE node_id=$1 ORDER BY created_at DESC LIMIT 1"
	var id any = uuid.MustParse(node)
	if options.Backend != "postgres" {
		query = "SELECT envelope FROM commands WHERE node_id=? ORDER BY created_at DESC LIMIT 1"
		id = mysql.UUIDBytes(uuid.MustParse(node))
	}
	var envelope []byte
	if err = db.Store.QueryRow(f.ctx, query, id).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	command := new(agentv1.CommandEnvelope)
	if err = proto.Unmarshal(envelope, command); err != nil {
		t.Fatal(err)
	}
	if command.ExpiresAt == nil || !command.ExpiresAt.AsTime().After(time.Now()) {
		t.Fatal("old authority probe must remain unexpired")
	}
	appendBytes := func(out []byte, field protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(out, field, protowire.BytesType), value)
	}
	requestID := uuid.Must(uuid.NewV7())
	request := appendBytes(nil, 1, requestID[:])
	request = protowire.AppendVarint(protowire.AppendTag(request, 2, protowire.VarintType), uint64(time.Now().Add(5*time.Second).UnixMilli()))
	accepted, _ := proto.Marshal(command.IssuedAt)
	request = appendBytes(request, 3, accepted)
	request = appendBytes(request, 7, envelope)
	request = protowire.AppendVarint(protowire.AppendTag(request, 8, protowire.VarintType), 1)
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, uint32(len(request)))
	frame = append(frame, request...)
	path := f.file("agent/old-authority-request", frame, 65533, 65533, 0600)
	response := f.run(65533, 65533, nil, "/usr/bin/python3", "-c", `import socket,struct,sys
s=socket.socket(socket.AF_UNIX);s.settimeout(5);s.connect('/run/ocserv-platform/privd.sock')
s.sendall(open(sys.argv[1],'rb').read())
def read(n):
 b=b''
 while len(b)<n:
  part=s.recv(n-len(b))
  if not part: raise RuntimeError('truncated privileged response')
  b+=part
 return b
size=struct.unpack('>I',read(4))[0]
assert 0<size<=1048576
sys.stdout.buffer.write(read(size))`, path)
	for len(response) > 0 {
		number, kind, n := protowire.ConsumeTag(response)
		if n < 0 {
			t.Fatal("malformed privileged response")
		}
		response = response[n:]
		if kind != protowire.BytesType {
			t.Fatal("unexpected privileged response field")
		}
		value, n := protowire.ConsumeBytes(response)
		if n < 0 {
			t.Fatal("malformed privileged result")
		}
		response = response[n:]
		if number == 20 {
			// PrivdError kind=PermissionDenied and the independent authority check's
			// diagnostic distinguish this from an arbitrary malformed request denial.
			if !bytes.HasPrefix(value, []byte{8, 2}) || (!bytes.Contains(value, []byte("node_id_mismatch")) && !bytes.Contains(value, []byte("key_unknown")) && !bytes.Contains(value, []byte("signature"))) {
				t.Fatalf("unexpected authority rejection: %q", value)
			}
			t.Log("privd rejected the unexpired original Controller command under the new binding")
			return
		}
	}
	t.Fatal("old Controller command was not rejected by privd")
}
