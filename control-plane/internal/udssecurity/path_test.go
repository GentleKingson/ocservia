package udssecurity

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

func privateSocketDirectory(t *testing.T) string {
	t.Helper()
	return testutil.PrivateTempDir(t)
}

func TestRequirePeerUIDRejectsUnauthorizedProcess(t *testing.T) {
	path := filepath.Join(privateSocketDirectory(t), "peer.sock")
	listener, err := net.Listen("unix", path)
	testutil.NoError(t, err)
	defer listener.Close()
	client, err := net.Dial("unix", path)
	testutil.NoError(t, err)
	defer client.Close()
	server, err := listener.Accept()
	testutil.NoError(t, err)
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
	testutil.NoError(t, err)
	testutil.NoError(t, os.Chmod(path, 0o660))
	identity, err := ValidateSocket(path, uint32(os.Geteuid()), uint32(os.Getegid()), 0o660)
	testutil.NoError(t, err)
	testutil.NoError(t, os.Remove(path))
	second, err := net.Listen("unix", path)
	testutil.NoError(t, err)
	defer second.Close()
	defer first.Close()
	if err := SameSocket(path, identity); err == nil {
		t.Fatal("replacement socket retained the original pathname identity")
	}
}

func TestValidateParentRejectsSymlinkAndWritableAncestry(t *testing.T) {
	directory := privateSocketDirectory(t)
	target := filepath.Join(directory, "target")
	testutil.NoError(t, os.Mkdir(target, 0o700))
	alias := filepath.Join(directory, "alias")
	testutil.NoError(t, os.Symlink(target, alias))
	if _, err := ValidateParent(filepath.Join(alias, "socket"), uint32(os.Geteuid())); err == nil {
		t.Fatal("symlink ancestry accepted")
	}
	testutil.NoError(t, os.Chmod(target, 0o770))
	if _, err := ValidateParent(filepath.Join(target, "socket"), uint32(os.Geteuid())); err == nil {
		t.Fatal("client-writable ancestry accepted")
	}
}
