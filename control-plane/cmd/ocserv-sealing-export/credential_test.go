package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
)

func TestCredentialFile(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".credential-test-")
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	directory, err = filepath.Abs(directory)
	testutil.NoError(t, err)
	directory, err = filepath.EvalSymlinks(directory)
	testutil.NoError(t, err)
	want := "postgres://app:fixture@postgres/ocservia\n"
	write := func(path, value string) {
		t.Helper()
		testutil.NoError(t, os.WriteFile(path, []byte(value), 0600))
	}
	key := filepath.Join(directory, "credential")
	write(key, want)
	for _, mode := range []os.FileMode{0600, 0400} {
		testutil.NoError(t, os.Chmod(key, mode))
		raw, err := readCredentialFile(key)
		if err != nil || string(raw) != want {
			t.Fatalf("protected credential mode %o rejected: %v", mode, err)
		}
	}
	check := func(name string, setup func(string) string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			child := filepath.Join(directory, name)
			testutil.NoError(t, os.Mkdir(child, 0700))
			if _, err := readCredentialFile(setup(child)); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
	check("relative", func(string) string { return "credential" })
	check("unclean", func(string) string { return directory + "/./credential" })
	check("root", func(string) string { return "/" })
	check("empty", func(child string) string { p := child + "/key"; write(p, ""); return p })
	check("oversize", func(child string) string { p := child + "/key"; write(p, strings.Repeat("x", 4097)); return p })
	check("symlink", func(child string) string {
		p := child + "/link"
		testutil.NoError(t, os.Symlink(key, p))
		return p
	})
	check("symlink-parent", func(child string) string {
		p := child + "/link"
		testutil.NoError(t, os.Symlink(directory, p))
		return p + "/credential"
	})
	check("hardlink", func(child string) string {
		p := child + "/key"
		write(p, want)
		testutil.NoError(t, os.Link(p, child+"/link"))
		return p
	})
	check("public-file", func(child string) string {
		p := child + "/key"
		write(p, want)
		testutil.NoError(t, os.Chmod(p, 0640))
		return p
	})
	check("writable-parent", func(child string) string {
		p := child + "/key"
		write(p, want)
		testutil.NoError(t, os.Chmod(child, 0770))
		return p
	})
	check("directory", func(child string) string { return child })
	check("fifo", func(child string) string {
		p := child + "/fifo"
		testutil.NoError(t, unix.Mkfifo(p, 0600))
		return p
	})
	if os.Geteuid() == 0 {
		for _, name := range []string{"foreign-file", "foreign-parent"} {
			check(name, func(child string) string {
				p := child + "/key"
				write(p, want)
				target := p
				if name == "foreign-parent" {
					target = child
				}
				testutil.NoError(t, os.Chown(target, 12345, -1))
				return p
			})
		}
	}
	t.Run("read-verified-descriptor", func(t *testing.T) {
		file, err := openCredentialFile(key)
		testutil.NoError(t, err)
		defer file.Close()
		testutil.NoError(t, os.Rename(key, key+".old"))
		write(key, "replacement")
		raw, err := io.ReadAll(file)
		if err != nil || string(raw) != want {
			t.Fatalf("read switched to replacement: %v", err)
		}
	})
}
