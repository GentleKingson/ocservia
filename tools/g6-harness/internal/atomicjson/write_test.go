package atomicjson

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(directory, "value.json")
	for _, value := range []string{"first", "<replacement>"} {
		if err := Write(path, map[string]string{"value": value}); err != nil {
			t.Fatal(err)
		}
	}
	want := "{\n  \"value\": \"<replacement>\"\n}\n"
	assertContents := func() {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("contents = %q, err = %v", got, err)
		}
	}
	assertContents()
	for name, mode := range map[string]os.FileMode{directory: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s permissions = %o, want %o", name, info.Mode().Perm(), mode)
		}
	}
	if err := Write(path, make(chan int)); err == nil {
		t.Fatal("expected encoding failure")
	}
	assertContents()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files remain: %v, err = %v", entries, err)
	}
}
