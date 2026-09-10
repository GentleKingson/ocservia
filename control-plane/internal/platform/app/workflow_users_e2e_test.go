package app

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"os"
	"strings"

	"github.com/google/uuid"
)

func (f *controllerE2E) waitOperation(id string) {
	f.t.Helper()
	f.wait("real command result "+id, func() bool {
		operation := f.api(f.admin, "GET", "/api/v1/operations/"+id, nil, nil, 200)
		switch operation["state"] {
		case "failed", "expired", "rejected", "unknown":
			f.t.Fatalf("real command did not succeed: state=%v error=%v", operation["state"], operation["error"])
		}
		return operation["state"] == "succeeded"
	})
}

func (f *controllerE2E) userWorkflow(nodePath string) {
	f.t.Helper()
	seal := func(password string) map[string]any {
		f.t.Helper()
		sealed, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &f.sealKeys["user_password"].PublicKey, []byte(password), nil)
		if err != nil {
			f.t.Fatal(err)
		}
		return map[string]any{"version": 1, "purpose": "user_password", "key_id": "e2e-user_password", "ciphertext": sealed}
	}
	password1, password2 := "real-E2E-first-user-password", "real-E2E-rotated-user-password"
	headers := map[string]string{"Idempotency-Key": uuid.NewString()}
	body := map[string]any{"name": "real-e2e-user", "expected_version": 0, "reason": "create native Ocserv account", "sealed_password": seal(password1)}
	op := f.api(f.admin, "POST", nodePath+"/users", body, headers, 202)
	id := e2eString(f.t, op, "id")
	if replay := f.api(f.admin, "POST", nodePath+"/users", body, headers, 202); replay["id"] != id {
		f.t.Fatal("user creation replay changed operation")
	}
	f.waitOperation(id)
	f.waitUserState(nodePath, "user", "real-e2e-user", true)
	f.authenticateOcserv(password1, true)
	// The Agent must not gain direct access to the privileged password file.
	if err := f.command(65533, 65533, nil, "/usr/bin/test", "-r", "/etc/ocserv/ocpasswd").Run(); err == nil {
		f.t.Fatal("Agent can read native password hashes")
	}
	if err := f.command(65533, 65533, nil, "/usr/bin/test", "-w", "/etc/ocserv/ocpasswd").Run(); err == nil {
		f.t.Fatal("Agent can write native password hashes")
	}
	mutate := func(action string, extra map[string]any) {
		f.t.Helper()
		state := f.userResource(nodePath, "user", "real-e2e-user")
		body := map[string]any{"expected_version": state["desired_version"], "reason": "verify native " + action}
		for key, value := range extra {
			body[key] = value
		}
		op := f.api(f.admin, "POST", nodePath+"/users/real-e2e-user:"+action, body, map[string]string{"Idempotency-Key": uuid.NewString()}, 202)
		f.waitOperation(e2eString(f.t, op, "id"))
	}
	mutate("rotate-password", map[string]any{"sealed_password": seal(password2)})
	f.authenticateOcserv(password1, false)
	f.authenticateOcserv(password2, true)
	mutate("disable", nil)
	f.waitUserState(nodePath, "user", "real-e2e-user", false)
	f.authenticateOcserv(password2, false)
	mutate("enable", nil)
	f.waitUserState(nodePath, "user", "real-e2e-user", true)
	f.authenticateOcserv(password2, true)
	group := f.api(f.admin, "PUT", nodePath+"/groups/real-e2e-group", map[string]any{"expected_version": 0, "members": []string{"real-e2e-user"}, "reason": "native group membership"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 202)
	f.waitOperation(e2eString(f.t, group, "id"))
	f.waitUserState(nodePath, "group", "real-e2e-group", true)
	passwordFile, err := os.ReadFile("/etc/ocserv/ocpasswd")
	if err != nil {
		f.t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(passwordFile), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 3 && fields[0] == "real-e2e-user" && fields[1] == "real-e2e-group" {
			found = true
		}
	}
	if !found {
		f.t.Fatal("native Ocserv password file does not contain approved group membership")
	}
	f.t.Log("real user creation, replay, password rotation, disable/enable, authentication and group convergence passed")
}

func (f *controllerE2E) userResource(nodePath, kind, name string) map[string]any {
	f.t.Helper()
	result := f.api(f.admin, "GET", nodePath+"/user-group-state", nil, nil, 200)
	items, ok := result["items"].([]any)
	if !ok {
		f.t.Fatal("user/group reader did not return items")
	}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if ok && item["kind"] == kind && item["name"] == name {
			return item
		}
	}
	f.t.Fatalf("user/group reader missing %s %s", kind, name)
	return nil
}

func (f *controllerE2E) waitUserState(nodePath, kind, name string, enabled bool) {
	f.t.Helper()
	f.wait("observed native "+kind+" convergence", func() bool {
		state := f.userResource(nodePath, kind, name)
		if state["convergence"] != "converged" {
			return false
		}
		if kind == "user" {
			return state["desired_enabled"] == enabled && state["observed_enabled"] == enabled
		}
		members, ok := state["observed_members"].([]any)
		return ok && len(members) == 1 && members[0] == "real-e2e-user"
	})
}

func (f *controllerE2E) authenticateOcserv(password string, success bool) {
	f.t.Helper()
	cmd := f.command(65533, 65533, nil, "/usr/bin/timeout", "15", "/usr/sbin/openconnect", "--authenticate", "--non-inter", "--protocol=anyconnect", "--user=real-e2e-user", "--passwd-on-stdin", "--cafile", f.root+"/relay-ca.pem", "https://127.0.0.1:44443")
	cmd.Stdin = strings.NewReader(password + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if success {
		if err != nil || !strings.Contains(stdout.String(), "COOKIE=") {
			f.t.Fatalf("native Ocserv authentication failed: %v", err)
		}
		return
	}
	if err == nil || cmd.ProcessState.ExitCode() == 124 || strings.Contains(stdout.String(), "COOKIE=") {
		f.t.Fatal("native Ocserv failed to reject obsolete/disabled credentials")
	}
	detail := strings.ToLower(stderr.String())
	if !strings.Contains(detail, "authentication") && !strings.Contains(detail, "obtain cookie") && !strings.Contains(detail, "401") {
		f.t.Fatal("negative login failed for a reason other than authentication rejection")
	}
}
