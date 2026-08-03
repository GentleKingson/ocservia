package trustserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenRejectsWritableParentDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	if listener, _, err := listen(filepath.Join(parent, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected writable trust socket parent to be rejected")
	}
}

func TestListenAcceptsPrivateOwnedParentDirectory(t *testing.T) {
	parent, err := os.MkdirTemp("/tmp", "trust-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	listener, identity, err := listen(filepath.Join(parent, "trust.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeSocket(filepath.Join(parent, "trust.sock"), identity); err != nil {
		t.Fatal(err)
	}
}

func TestListenRejectsSymlinkParentDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if listener, _, err := listen(filepath.Join(alias, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected symlink trust socket parent to be rejected")
	}
}

func TestListenRejectsWritableAncestorDirectory(t *testing.T) {
	ancestor := t.TempDir()
	if err := os.Chmod(ancestor, 0o777); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(ancestor, "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if listener, _, err := listen(filepath.Join(parent, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected writable trust socket ancestor to be rejected")
	}
}
