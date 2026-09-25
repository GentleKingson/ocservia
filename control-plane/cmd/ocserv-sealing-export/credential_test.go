package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCredentialFile(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".credential-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	directory, err = filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://app:fixture@postgres/ocservia\n"
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(directory, "credential")
	write(key, want)
	for _, mode := range []os.FileMode{0600, 0400} {
		if err := os.Chmod(key, mode); err != nil {
			t.Fatal(err)
		}
		raw, err := readCredentialFile(key)
		if err != nil || string(raw) != want {
			t.Fatalf("protected credential mode %o rejected: %v", mode, err)
		}
	}
	check := func(name string, setup func(string) string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			child := filepath.Join(directory, name)
			if err := os.Mkdir(child, 0700); err != nil {
				t.Fatal(err)
			}
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
		if err := os.Symlink(key, p); err != nil {
			t.Fatal(err)
		}
		return p
	})
	check("symlink-parent", func(child string) string {
		p := child + "/link"
		if err := os.Symlink(directory, p); err != nil {
			t.Fatal(err)
		}
		return p + "/credential"
	})
	check("hardlink", func(child string) string {
		p := child + "/key"
		write(p, want)
		if err := os.Link(p, child+"/link"); err != nil {
			t.Fatal(err)
		}
		return p
	})
	check("public-file", func(child string) string {
		p := child + "/key"
		write(p, want)
		if err := os.Chmod(p, 0640); err != nil {
			t.Fatal(err)
		}
		return p
	})
	check("writable-parent", func(child string) string {
		p := child + "/key"
		write(p, want)
		if err := os.Chmod(child, 0770); err != nil {
			t.Fatal(err)
		}
		return p
	})
	check("directory", func(child string) string { return child })
	check("fifo", func(child string) string {
		p := child + "/fifo"
		if err := unix.Mkfifo(p, 0600); err != nil {
			t.Fatal(err)
		}
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
				if err := os.Chown(target, 12345, -1); err != nil {
					t.Fatal(err)
				}
				return p
			})
		}
	}
	t.Run("read-verified-descriptor", func(t *testing.T) {
		file, err := openCredentialFile(key)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := os.Rename(key, key+".old"); err != nil {
			t.Fatal(err)
		}
		write(key, "replacement")
		raw, err := io.ReadAll(file)
		if err != nil || string(raw) != want {
			t.Fatalf("read switched to replacement: %v", err)
		}
	})
}
