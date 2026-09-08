package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalBootstrapConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	const password = "bootstrap-test-secret"
	if err := os.WriteFile(path, []byte(password+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"OCSERV_DATABASE_URL":                  "postgres://db/test",
		"OCSERV_LOCAL_AUTH_ENABLED":            "true",
		"OCSERV_SESSION_KEY":                   strings.Repeat("a", 64),
		"OCSERV_LOCAL_BOOTSTRAP_USERNAME":      "first-admin",
		"OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID":  "01900000-0000-7000-8000-000000000001",
		"OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE": path,
	}
	lookup := func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	cfg, err := Load([]string{"--bootstrap-local-admin"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BootstrapLocalAdmin || cfg.LocalBootstrapPassword != password {
		t.Fatal("bootstrap secret file not loaded")
	}
	cfg, err = Load(nil, lookup)
	if err != nil || cfg.LocalBootstrapPassword != "" || cfg.BootstrapLocalAdmin {
		t.Fatal("normal startup loaded bootstrap credential")
	}
	for _, args := range [][]string{{"--bootstrap-local-admin", "--migrate-only"}, {"--bootstrap-local-admin", "--schema-compatibility-check=32"}, {"--password=" + password}, {password}} {
		_, err := Load(args, lookup)
		if err == nil || strings.Contains(err.Error(), password) {
			t.Fatal("invalid CLI accepted or disclosed password")
		}
	}
	values["OCSERV_LOCAL_BOOTSTRAP_PASSWORD"] = password
	if _, err := Load([]string{"--bootstrap-local-admin"}, lookup); err == nil || strings.Contains(err.Error(), password) {
		t.Fatal("plaintext env accepted or disclosed")
	}
	delete(values, "OCSERV_LOCAL_BOOTSTRAP_PASSWORD")
	values["OCSERV_LOCAL_AUTH_ENABLED"] = "false"
	if _, err := Load([]string{"--bootstrap-local-admin"}, lookup); err == nil {
		t.Fatal("bootstrap without Local auth accepted")
	}
	values["OCSERV_LOCAL_AUTH_ENABLED"] = "true"
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	values["OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE"] = link
	if _, err := Load([]string{"--bootstrap-local-admin"}, lookup); err == nil {
		t.Fatal("symlink secret accepted")
	}
	delete(values, "OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE")
	if _, err := Load([]string{"--bootstrap-local-admin"}, lookup); err == nil {
		t.Fatal("missing password accepted")
	}
}
