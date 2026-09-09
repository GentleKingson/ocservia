package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFoundationSecretFile(t *testing.T) {
	dsn := "user:private-password@tcp(127.0.0.1:3306)/test?tls=false"
	path := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := func(key string) (string, bool) {
		if key == "OCSERV_DATABASE_URL_FILE" {
			return path, true
		}
		return "", false
	}
	got, err := FoundationDatabaseURL(lookup)
	if err != nil || got != dsn {
		t.Fatal("private DSN file rejected")
	}
	if _, err = FoundationDatabaseURL(func(key string) (string, bool) {
		if key == "OCSERV_DATABASE_URL" {
			return "", true
		}
		return lookup(key)
	}); err == nil {
		t.Fatal("inline/file conflict accepted")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = FoundationDatabaseURL(lookup)
	if err == nil || strings.Contains(err.Error(), dsn) || strings.Contains(err.Error(), path) {
		t.Fatal("insecure file accepted or secret path leaked")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = FoundationDatabaseURL(func(key string) (string, bool) {
		if key == "OCSERV_DATABASE_URL_FILE" {
			return link, true
		}
		return "", false
	}); err == nil {
		t.Fatal("symlink accepted")
	}
}
