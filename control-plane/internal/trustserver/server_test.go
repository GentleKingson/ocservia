package trustserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

func TestStaleSocketErrorRequiresDefinitiveFailure(t *testing.T) {
	if !staleSocketError(syscall.ECONNREFUSED) || !staleSocketError(os.ErrNotExist) {
		t.Fatal("expected refused and missing sockets to be stale")
	}
	if staleSocketError(context.DeadlineExceeded) || staleSocketError(errors.New("ambiguous probe failure")) {
		t.Fatal("expected ambiguous probe failures to preserve the socket")
	}
}

func TestListenRejectsWritableParentDirectory(t *testing.T) {
	parent := t.TempDir()
	testutil.NoError(t, os.Chmod(parent, 0o770))
	if listener, _, err := listen(filepath.Join(parent, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected writable trust socket parent to be rejected")
	}
}

func TestListenAcceptsPrivateOwnedParentDirectory(t *testing.T) {
	parent := testutil.PrivateTempDir(t)
	listener, identity, err := listen(filepath.Join(parent, "trust.sock"))
	testutil.NoError(t, err)
	testutil.NoError(t, listener.Close())
	testutil.NoError(t, removeSocket(filepath.Join(parent, "trust.sock"), identity))
}

func TestListenRejectsSymlinkParentDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	testutil.NoError(t, os.Mkdir(target, 0o700))
	alias := filepath.Join(root, "alias")
	testutil.NoError(t, os.Symlink(target, alias))
	if listener, _, err := listen(filepath.Join(alias, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected symlink trust socket parent to be rejected")
	}
}

func TestListenRejectsWritableAncestorDirectory(t *testing.T) {
	ancestor := t.TempDir()
	testutil.NoError(t, os.Chmod(ancestor, 0o777))
	parent := filepath.Join(ancestor, "private")
	testutil.NoError(t, os.Mkdir(parent, 0o700))
	if listener, _, err := listen(filepath.Join(parent, "trust.sock")); err == nil {
		_ = listener.Close()
		t.Fatal("expected writable trust socket ancestor to be rejected")
	}
}
