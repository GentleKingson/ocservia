package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/mysql"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/value"
	"github.com/google/uuid"
)

// Run only in the disposable, unexposed Linux container launched by
// scripts/database-controller-e2e.sh on BuildServer. No application service,
// transport, Agent, root receipt, or privileged command is replaced by a stub.
func TestControllerTransportBackendE2E(t *testing.T) {
	if os.Getenv("PR02_CONTROLLER_E2E") != "1" {
		t.Skip("requires the isolated real-process database E2E container")
	}
	if os.Geteuid() != 0 {
		t.Fatal("container root is required to launch the distinct runtime principals")
	}
	f := newControllerE2E(t)
	ownerOptions, runtimeOptions, account := controllerProcessDatabase(t)
	f.run(0, 0, nil, "go", "build", "-buildvcs=false", "-o", f.root+"/ocserv-control", "../../../cmd/ocserv-control")
	f.run(0, 0, f.environment(ownerOptions, map[string]string{"OCSERV_RUNTIME_DATABASE_ROLE": account}), f.root+"/ocserv-control", "--migrate-only")
	owner, err := connection.Open(f.ctx, ownerOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
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
	f.startRelays()
	sealArgs := f.prepareSealKeys()
	signer := f.startSigner()
	f.startOcserv()
	address := e2eAddress(t)
	f.base = "http://" + address
	f.start("controller", 65534, 65532, f.environment(runtimeOptions, map[string]string{
		"OCSERV_HTTP_ADDRESS": address, "OCSERV_PUBLIC_ORIGIN": f.base,
		"OCSERV_CONTROLLER_ENDPOINT_ID": endpointID, "OCSERV_COMMAND_SIGNING_KEY_FILE": f.root + "/controller/signing.pem",
		"OCSERV_TRANSPORT_SOCKET": f.root + "/transport/control.sock", "OCSERV_TRUST_SOCKET": f.root + "/controller/trust.sock",
		"OCSERV_TRANSPORT_UID": "65532", "OCSERV_TRANSPORT_GID": "65532",
		"OCSERV_CERTIFICATE_SIGNER_URL": signer.URL, "OCSERV_CERTIFICATE_SIGNER_TOKEN": "isolated-test-signer",
		"SSL_CERT_FILE": f.root + "/signer-ca.pem",
	}), f.root+"/ocserv-control")
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

	identityArgs := []string{"--identity-dir", f.root + "/agent/identity", "--controller", endpointID}
	prepared := f.run(65533, 65533, nil, "/usr/local/bin/ocservia-agent", append(append([]string{}, identityArgs...), "--prepare-enrollment")...)
	agentEndpoint := strings.TrimSpace(string(prepared))
	if decoded, err := hex.DecodeString(agentEndpoint); err != nil || len(decoded) != 32 {
		t.Fatal("Agent did not produce its authenticated persistent endpoint identity")
	}
	token := f.api(f.admin, "POST", "/api/v1/enrollment-tokens", map[string]any{"workspace_id": f.workspace, "environment": "development", "expected_node_name": "real-e2e-node", "expected_endpoint_id": agentEndpoint, "ttl_seconds": 600, "reason": "real three-backend process acceptance"}, nil, http.StatusCreated)
	f.file("agent/enrollment-token", []byte(e2eString(t, token, "token")), 65533, 65533, 0600)
	agentArgs := append(append([]string{}, identityArgs...), f.relayArgs("agent")...)
	agentArgs = append(agentArgs, sealArgs...)
	enrollArgs := append(append([]string{}, agentArgs...), "--enrollment-token-file", f.root+"/agent/enrollment-token", "--enrollment-environment", "development")
	enrolled := f.run(65533, 65533, nil, "/usr/local/bin/ocservia-agent", enrollArgs...)
	nodeID := ""
	for _, line := range strings.Split(string(enrolled), "\n") {
		if id, err := uuid.Parse(strings.TrimSpace(line)); err == nil && id.Version() == 7 {
			if nodeID != "" {
				t.Fatal("Agent enrollment returned multiple node identities")
			}
			nodeID = id.String()
		}
	}
	if nodeID == "" {
		t.Fatal("Agent enrollment did not return a UUIDv7 node")
	}
	nodePath := "/api/v1/nodes/" + nodeID
	capabilities := []string{"ocserv.config_fingerprint.read", "ocserv.ip_bans.read", "ocserv.sessions.read", "ocserv.status.read", "ocserv.version.read", "ocserv.fencing.v2", "command.semantic-hash.v1", "command.strict-wire.v1", "privd_result_attestation_v1", "ocserv.certificate.issue", "ocserv.certificate.revoke", "ocserv.users.write", "ocserv.groups.write", "ocserv.config.plan", "ocserv.config.apply", "ocserv.service.reload"}
	trust := map[string]any{"labels": map[string]string{"fixture": "real-process"}, "policy": "default", "capabilities": capabilities}
	approval := f.approve("node.approve", "node", nodeID, map[string]any{"node_approval": trust})
	trust["reason"] = "activate authenticated real Agent"
	f.api(f.admin, "POST", nodePath+"/approval", trust, map[string]string{"X-Approval-ID": approval}, http.StatusOK, http.StatusServiceUnavailable)
	credential := f.api(f.admin, "POST", nodePath+"/privd-attestation-credentials", map[string]any{"ttl_seconds": 300, "reason": "provision real root key"}, nil, http.StatusCreated)
	attestationKey := f.root + "/privd/state/attestation.key"
	registration := f.run(0, 65533, nil, "/usr/local/bin/ocservia-privd", "attestation-registration", attestationKey, nodeID, e2eString(t, credential, "controller_nonce_hex"), e2eString(t, credential, "credential_context_sha256_hex"))
	var body map[string]any
	if err := json.Unmarshal(registration, &body); err != nil {
		t.Fatal("decode root registration", err)
	}
	body["credential"] = e2eString(t, credential, "credential")
	f.api(nil, "POST", nodePath+"/privd-attestation-keys:register", body, nil, http.StatusCreated)
	f.api(nil, "POST", nodePath+"/privd-attestation-keys:register", body, nil, http.StatusUnauthorized)
	privdArgs := []string{"--socket", f.root + "/privd/privd.sock", "--agent-uid", "65533", "--node-id", nodeID, "--controller-command-key-file", f.root + "/privd/verification.pem", "--attestation-key-file", attestationKey, "--user-password-seal-key-file", f.root + "/privd/user.key", "--p12-password-seal-key-file", f.root + "/privd/p12.key"}
	privdArgs = append(privdArgs, sealArgs...)
	f.start("privd", 0, 65533, nil, "/usr/local/bin/ocservia-privd", privdArgs...)
	f.wait("root supervisor socket", func() bool { _, err := os.Stat(f.root + "/privd/privd.sock"); return err == nil })
	agentArgs = append(agentArgs, "--journal", f.root+"/agent/journal.db", "--node-id", nodeID, "--privd-socket", f.root+"/privd/privd.sock", "--controller-command-key-file", f.root+"/agent/verification.pem")
	f.start("agent", 65533, 65533, nil, "/usr/local/bin/ocservia-agent", agentArgs...)
	f.wait("fenced Agent connected with persisted telemetry", func() bool {
		node := f.api(f.admin, "GET", nodePath, nil, nil, http.StatusOK)
		return node["trust_status"] == "active" && node["connection_state"] == "online" && node["observed_at"] != nil
	})
	f.requireFencedSession()
	f.certificateWorkflow(nodePath)
	f.userWorkflow(nodePath)
	for _, path := range []string{nodePath + "/sessions", nodePath + "/ip-bans", "/api/v1/operations", "/api/v1/operations/summary", "/api/v1/operations/queue-metrics", "/api/v1/events", "/api/v1/audit/events"} {
		f.api(f.admin, "GET", path, nil, nil, http.StatusOK)
	}
	verified := f.api(f.admin, "POST", "/api/v1/audit:verify", nil, nil, http.StatusOK)
	if verified["valid"] != true || verified["checkpoint_valid"] != true {
		t.Fatal("real workflow audit chain/checkpoint verification failed")
	}
	t.Log("real CLI, independent local identities, two dedicated TLS relays, enrollment, root attestation, fenced certificate lifecycle and one-use artifact passed")
}

type controllerE2E struct {
	t                                *testing.T
	ctx                              context.Context
	root, artifacts, workspace, base string
	client                           *http.Client
	admin, approver                  *http.Cookie
	relays                           []string
	sealKeys                         map[string]*rsa.PrivateKey
	processes                        []*e2eProcess
}

type e2eProcess struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func newControllerE2E(t *testing.T) *controllerE2E {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
	t.Cleanup(cancel)
	f := &controllerE2E{t: t, ctx: ctx, root: "/run/pr02-e2e", artifacts: os.Getenv("OCSERV_E2E_ARTIFACT_DIR"), client: &http.Client{Timeout: 10 * time.Second}}
	if f.artifacts == "" {
		t.Fatal("isolated artifact directory required")
	}
	for path, ids := range map[string][2]int{"": {0, 0}, "controller": {65534, 65532}, "transport": {65532, 65532}, "agent": {65533, 65533}, "privd": {0, 65533}, "privd/state": {0, 0}} {
		dir := filepath.Join(f.root, path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if path != "" {
			mode := os.FileMode(0750)
			if path == "privd/state" {
				mode = 0700
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chown(dir, ids[0], ids[1]); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *controllerE2E) file(name string, data []byte, uid, gid int, mode os.FileMode) string {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, data, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *controllerE2E) environment(options connection.Options, extra map[string]string) []string {
	values := map[string]string{"OCSERV_ENVIRONMENT": "test", "OCSERV_DATABASE_BACKEND": options.Backend, "OCSERV_DATABASE_URL": options.URL,
		"OCSERV_AUDIT_EVENT_KEY_ID": "e2e-audit-v1", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32), "OCSERV_AUDIT_CHECKPOINT_KEY": strings.Repeat("22", 32), "OCSERV_SESSION_KEY": strings.Repeat("33", 32), "OCSERV_LOCAL_AUTH_ENABLED": "true", "OCSERV_RECOMMENDED_AGENT_VERSION": "1.2.3"}
	for key, v := range extra {
		values[key] = v
	}
	var env []string
	for key, v := range values {
		env = append(env, key+"="+v)
	}
	return env
}

func (f *controllerE2E) command(uid, gid int, env []string, binary string, args ...string) *exec.Cmd {
	f.t.Helper()
	command := append([]string{"--reuid", strconv.Itoa(uid), "--regid", strconv.Itoa(gid), "--clear-groups", binary}, args...)
	cmd := exec.CommandContext(f.ctx, "/usr/bin/setpriv", command...)
	// Do not inherit owner/admin database credentials into any child process.
	cmd.Env = []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/tmp", "GOCACHE=" + os.Getenv("GOCACHE"), "GOMODCACHE=" + os.Getenv("GOMODCACHE"), "GOPROXY=off", "GOTOOLCHAIN=local", "RUST_LOG=info"}
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func (f *controllerE2E) run(uid, gid int, env []string, binary string, args ...string) []byte {
	f.t.Helper()
	cmd := f.command(uid, gid, env, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		f.t.Fatalf("%s failed: %v\n%s", filepath.Base(binary), err, stderr.String())
	}
	return stdout.Bytes()
}

func (f *controllerE2E) start(name string, uid, gid int, env []string, binary string, args ...string) {
	f.t.Helper()
	log, err := os.OpenFile(filepath.Join(f.artifacts, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := f.command(uid, gid, env, binary, args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		f.t.Fatal(err)
	}
	p := &e2eProcess{name: name, cmd: cmd, done: make(chan struct{})}
	f.processes = append(f.processes, p)
	go func() { p.err = cmd.Wait(); close(p.done) }()
	f.t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-p.done
		}
		_ = log.Close()
	})
}

func (f *controllerE2E) wait(description string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range f.processes {
			select {
			case <-p.done:
				f.t.Fatalf("%s exited before %s: %v (see process log)", p.name, description, p.err)
			default:
			}
		}
		if ready() {
			return
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	f.t.Fatalf("timed out waiting for %s (see process logs)", description)
}

func (f *controllerE2E) requireFencedSession() {
	f.t.Helper()
	log, err := os.Open(filepath.Join(f.artifacts, "agent.log"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer log.Close()
	decoder := json.NewDecoder(log)
	for {
		var record struct {
			Fields map[string]any `json:"fields"`
		}
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			f.t.Fatal("decode Agent session evidence", err)
		}
		if record.Fields["message"] == "agent session accepted" && record.Fields["session_mode"] == "fenced_v1_1" {
			return
		}
	}
	f.t.Fatal("real Agent did not verify and activate a fenced session")
}

func e2eAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func (f *controllerE2E) request(cookie *http.Cookie, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	f.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(f.ctx, method, f.base+path, bytes.NewReader(encoded))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", f.base)
	req.Header.Set("X-Workspace-ID", f.workspace)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, err := f.client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	_ = response.Body.Close()
	if err != nil {
		f.t.Fatal(err)
	}
	return response, data
}

func (f *controllerE2E) api(cookie *http.Cookie, method, path string, body any, headers map[string]string, statuses ...int) map[string]any {
	f.t.Helper()
	response, data := f.request(cookie, method, path, body, headers)
	ok := false
	for _, status := range statuses {
		ok = ok || response.StatusCode == status
	}
	if !ok {
		var problem struct{ Title, Detail string }
		_ = json.Unmarshal(data, &problem)
		f.t.Fatalf("%s %s: status %d, expected %v; %s: %s", method, path, response.StatusCode, statuses, problem.Title, problem.Detail)
	}
	var decoded map[string]any
	if len(data) != 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			f.t.Fatalf("decode %s: %v", path, err)
		}
	}
	return decoded
}

func e2eString(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(string)
	if !ok || value == "" {
		t.Fatalf("missing nonempty %s", key)
	}
	return value
}

func (f *controllerE2E) login(username, password string) *http.Cookie {
	f.t.Helper()
	response, _ := f.request(nil, "POST", "/api/v1/auth/login", map[string]string{"username": username, "password": password}, nil)
	if response.StatusCode != http.StatusNoContent || len(response.Cookies()) != 1 {
		f.t.Fatalf("real local login rejected: %d", response.StatusCode)
	}
	return response.Cookies()[0]
}

func (f *controllerE2E) approve(action, resourceType, id string, extra map[string]any) string {
	f.t.Helper()
	body := map[string]any{"action": action, "resource_type": resourceType, "resource_id": id, "ttl_seconds": 600, "reason": "independent real process acceptance approval"}
	for k, v := range extra {
		body[k] = v
	}
	approval := f.api(f.admin, "POST", "/api/v1/approval-requests", body, nil, http.StatusCreated)
	approvalID := e2eString(f.t, approval, "id")
	decision := map[string]any{"reason": "independently reviewed exact content", "expected_request_hash": e2eString(f.t, approval, "request_hash")}
	f.api(f.admin, "POST", "/api/v1/approval-requests/"+approvalID+":approve", decision, nil, http.StatusForbidden)
	f.api(f.approver, "POST", "/api/v1/approval-requests/"+approvalID+":approve", decision, nil, http.StatusOK)
	return approvalID
}

func (f *controllerE2E) startRelays() {
	f.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated E2E relay CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		f.t.Fatal(err)
	}
	f.file("relay-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0, 0, 0644)
	cert := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "isolated E2E relay"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
	if err != nil {
		f.t.Fatal(err)
	}
	f.file("relay.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0, 0, 0644)
	f.file("relay.key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0, 0, 0600)
	for name, ids := range map[string][2]int{"agent": {65533, 65533}, "transport": {65532, 65532}} {
		f.file(name+"/relay-token", []byte("real-process-private-relay-token-v1"), ids[0], ids[1], 0600)
	}
	for i := 0; i < 2; i++ {
		address := e2eAddress(f.t)
		config := fmt.Sprintf("enable_relay = true\nhttp_bind_addr = %q\nenable_quic_addr_discovery = false\nenable_metrics = false\n[access]\nshared_token = [\"real-process-private-relay-token-v1\"]\n[tls]\nhttps_bind_addr = %q\ncert_mode = \"Manual\"\nmanual_cert_path = %q\nmanual_key_path = %q\n", e2eAddress(f.t), address, f.root+"/relay.pem", f.root+"/relay.key")
		path := f.file(fmt.Sprintf("relay-%d.toml", i), []byte(config), 0, 0, 0600)
		f.start(fmt.Sprintf("relay-%d", i), 0, 0, nil, "/usr/local/bin/iroh-relay", "--config-path", path)
		f.relays = append(f.relays, "https://"+address)
		f.wait("dedicated TLS relay listener", func() bool {
			conn, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		})
	}
}

func (f *controllerE2E) relayArgs(principal string) []string {
	return []string{"--relay-mode", "custom", "--relay-url", f.relays[0], "--relay-url", f.relays[1], "--relay-token-file", f.root + "/" + principal + "/relay-token", "--relay-ca-file", f.root + "/relay-ca.pem"}
}

func (f *controllerE2E) prepareSealKeys() []string {
	f.t.Helper()
	f.sealKeys = map[string]*rsa.PrivateKey{}
	var args []string
	for _, item := range []struct{ name, purpose, flag string }{{"user", "user_password", "user-password"}, {"p12", "certificate_p12_password", "p12-password"}} {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			f.t.Fatal(err)
		}
		f.sealKeys[item.purpose] = key
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		f.file("privd/"+item.name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0, 0, 0600)
		publicDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		digest := sha256.Sum256(publicDER)
		args = append(args, "--"+item.flag+"-seal-key-id", "e2e-"+item.purpose, "--"+item.flag+"-seal-public-key-sha256", hex.EncodeToString(digest[:]))
	}
	return args
}

func (f *controllerE2E) startSigner() *httptest.Server {
	f.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "isolated E2E certificate CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		f.t.Fatal(err)
	}
	var mu sync.Mutex
	issued := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer isolated-test-signer" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			var request struct {
				ID  string `json:"certificate_id"`
				CSR []byte `json:"csr_der"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.ID == "" || r.Header.Get("Idempotency-Key") != request.ID {
				http.Error(w, "invalid signing request", 400)
				return
			}
			csr, err := x509.ParseCertificateRequest(request.CSR)
			if err != nil || csr.CheckSignature() != nil {
				http.Error(w, "invalid CSR", 400)
				return
			}
			chain := issued[request.ID]
			if chain == "" {
				serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
				if err != nil {
					http.Error(w, "entropy", 500)
					return
				}
				leaf := &x509.Certificate{SerialNumber: serial, RawSubject: csr.RawSubject, Subject: csr.Subject, DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
				der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, key)
				if err != nil {
					http.Error(w, "signing", 500)
					return
				}
				chain = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
				issued[request.ID] = chain
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"certificate_chain_pem": chain})
		case "/seal":
			purpose := r.Header.Get("X-Ocservia-Seal-Purpose")
			key := f.sealKeys[purpose]
			plaintext, err := io.ReadAll(io.LimitReader(r.Body, 1025))
			if key == nil || err != nil || len(plaintext) == 0 || len(plaintext) > 1024 {
				http.Error(w, "invalid sealing request", 400)
				return
			}
			sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, plaintext, nil)
			if err != nil {
				http.Error(w, "sealing", 500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sealed": sealed, "key_id": "e2e-" + purpose, "version": 1, "purpose": purpose})
		case "/revoke":
			var request struct {
				ID     string `json:"certificate_id"`
				Serial string `json:"serial_number"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || issued[request.ID] == "" || request.Serial == "" {
				http.Error(w, "unknown certificate", 400)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	f.t.Cleanup(server.Close)
	f.file("signer-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0, 0, 0644)
	return server
}

func (f *controllerE2E) startOcserv() {
	f.t.Helper()
	if err := os.MkdirAll("/run/ocserv", 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile("/etc/ocserv/ocpasswd", nil, 0600); err != nil {
		f.t.Fatal(err)
	}
	config := fmt.Sprintf("auth = \"plain[passwd=/etc/ocserv/ocpasswd]\"\ntcp-port = 44443\nudp-port = 0\nlisten-host = 127.0.0.1\nrun-as-user = nobody\nrun-as-group = ocservia-control\nsocket-file = /run/ocserv/ocserv.sock\nocctl-socket-file = /run/occtl.socket\npid-file = /run/ocserv.pid\nserver-cert = %s/relay.pem\nserver-key = %s/relay.key\nisolate-workers = false\nmax-clients = 4\nmax-same-clients = 2\nkeepalive = 30\ndpd = 30\nuse-occtl = true\ndevice = vpns-pr02\nipv4-network = 10.250.0.0\nipv4-netmask = 255.255.255.0\ndns = 192.0.2.53\n", f.root, f.root)
	if err := os.WriteFile("/etc/ocserv/ocserv.conf", []byte(config), 0600); err != nil {
		f.t.Fatal(err)
	}
	f.run(0, 0, nil, "/usr/sbin/ocserv", "--test-config", "-c", "/etc/ocserv/ocserv.conf")
	f.start("ocserv", 0, 0, nil, "/usr/sbin/ocserv", "-f", "-d", "2", "-c", "/etc/ocserv/ocserv.conf")
	f.wait("real Ocserv listener", func() bool {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:44443", time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func (f *controllerE2E) certificateWorkflow(nodePath string) {
	f.t.Helper()
	nodeVersion := func() any { return f.api(f.admin, "GET", nodePath, nil, nil, 200)["version"] }
	create := map[string]any{"expected_version": nodeVersion(), "common_name": "real-e2e-client", "dns_names": []string{"client.example.test"}, "key_bits": 2048, "reason": "real root key and CSR generation"}
	headers := map[string]string{"Idempotency-Key": uuid.NewString()}
	certificate := f.api(f.admin, "POST", nodePath+"/certificates", create, headers, http.StatusAccepted)
	id := e2eString(f.t, certificate, "id")
	path := "/api/v1/certificates/" + id
	replayed := f.api(f.admin, "POST", nodePath+"/certificates", create, headers, http.StatusAccepted)
	if replayed["id"] != id {
		f.t.Fatal("certificate creation replay changed identity")
	}
	f.wait("root-attested CSR ready", func() bool {
		certificate = f.api(f.admin, "GET", path, nil, nil, 200)
		return certificate["state"] == "csr_ready"
	})
	approval := f.approve("certificate.issue", "certificate", id, nil)
	certificate = f.api(f.admin, "POST", path+":issue", map[string]any{"approval_id": approval, "reason": "sign verified root CSR"}, nil, 200)
	if certificate["state"] != "issued" {
		f.t.Fatal("certificate was not issued")
	}
	reason := "export one-time P12 after independent approval"
	artifactID := uuid.Must(uuid.NewV7()).String()
	approval = f.approve("certificate.private_key.export", "certificate", id, map[string]any{"certificate": map[string]any{"expected_version": certificate["version"], "purpose": "certificate_p12", "artifact_request_id": artifactID, "reason": reason}})
	grant := f.api(f.admin, "POST", path+":p12", map[string]any{"expected_version": nodeVersion(), "certificate_version": certificate["version"], "approval_id": approval, "reason": reason}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusAccepted)
	operation, ok := grant["operation"].(map[string]any)
	if !ok {
		f.t.Fatal("P12 operation missing")
	}
	operationID := e2eString(f.t, operation, "id")
	f.wait("actual root P12 operation", func() bool {
		op := f.api(f.admin, "GET", "/api/v1/operations/"+operationID, nil, nil, 200)
		if op["state"] == "failed" || op["state"] == "expired" {
			f.t.Fatalf("P12 operation terminated: %v", op["state"])
		}
		return op["state"] == "succeeded"
	})
	artifactPath := "/api/v1/artifacts/" + e2eString(f.t, grant, "artifact_id")
	downloadHeaders := map[string]string{"X-Artifact-Token": e2eString(f.t, grant, "download_token")}
	response, blob := f.request(f.admin, "GET", artifactPath, nil, downloadHeaders)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/x-pkcs12" {
		f.t.Fatalf("artifact download: %d", response.StatusCode)
	}
	p12Path := f.file("download.p12", blob, 0, 0, 0600)
	passwordPath := f.file("p12-password", []byte(e2eString(f.t, grant, "password")), 0, 0, 0600)
	f.run(0, 0, nil, "/usr/bin/openssl", "pkcs12", "-in", p12Path, "-passin", "file:"+passwordPath, "-noout")
	f.api(f.admin, "GET", artifactPath, nil, downloadHeaders, http.StatusForbidden)
	reason = "revoke real issued certificate"
	approval = f.approve("certificate.revoke", "certificate", id, map[string]any{"certificate": map[string]any{"expected_version": certificate["version"], "reason": reason}})
	f.api(f.admin, "POST", path+":revoke", map[string]any{"expected_version": nodeVersion(), "certificate_version": certificate["version"], "approval_id": approval, "reason": reason}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusAccepted)
	f.wait("root certificate revocation", func() bool { return f.api(f.admin, "GET", path, nil, nil, 200)["state"] == "revoked" })
	list := f.api(f.admin, "GET", nodePath+"/certificates", nil, nil, 200)
	encoded, _ := json.Marshal(list)
	var decoded struct {
		Items []certificates.Certificate `json:"items"`
	}
	if json.Unmarshal(encoded, &decoded) != nil || len(decoded.Items) != 1 || decoded.Items[0].State != "revoked" {
		f.t.Fatal("certificate list did not reflect completed lifecycle")
	}
	f.t.Log("real root-attested CSR, independent issue/export/revoke approvals, signed chain, P12 and one-use artifact passed")
}
