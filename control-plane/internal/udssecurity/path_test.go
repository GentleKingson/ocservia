package udssecurity

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func privateSocketDirectory(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(workingDirectory, ".uds-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestRequirePeerUIDRejectsUnauthorizedProcess(t *testing.T) {
	path := filepath.Join(privateSocketDirectory(t), "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if err := RequirePeerUID(server, uint32(os.Geteuid())); err != nil {
		t.Fatalf("current peer rejected: %v", err)
	}
	if err := RequirePeerUID(server, uint32(os.Geteuid()+1)); err == nil {
		t.Fatal("foreign peer UID was accepted")
	}
}

func TestSameSocketRejectsPathReplacement(t *testing.T) {
	path := filepath.Join(privateSocketDirectory(t), "identity.sock")
	first, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	identity, err := ValidateSocket(path, uint32(os.Geteuid()), uint32(os.Getegid()), 0o660)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	defer first.Close()
	if err := SameSocket(path, identity); err == nil {
		t.Fatal("replacement socket retained the original pathname identity")
	}
}

func TestValidateParentRejectsSymlinkAndWritableAncestry(t *testing.T) {
	directory := privateSocketDirectory(t)
	target := filepath.Join(directory, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateParent(filepath.Join(alias, "socket"), uint32(os.Geteuid())); err == nil {
		t.Fatal("symlink ancestry accepted")
	}
	if err := os.Chmod(target, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateParent(filepath.Join(target, "socket"), uint32(os.Geteuid())); err == nil {
		t.Fatal("client-writable ancestry accepted")
	}
}
