package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateTempDirRejectsUnsafeTMPDIRAndAvoidsRepo(t *testing.T) {
	unsafe := t.TempDir()
	NoError(t, os.Chmod(unsafe, 0o777))
	t.Setenv("TMPDIR", unsafe)

	dir := PrivateTempDir(t)
	if !filepath.IsAbs(dir) {
		t.Fatalf("expected absolute path, got %q", dir)
	}
	if !hasSafeAncestry(dir, uint32(os.Geteuid())) {
		t.Fatalf("PrivateTempDir returned path with unsafe ancestry: %q", dir)
	}
	if repoRoot := findRepoRoot(); repoRoot != "" && isWithinDir(dir, repoRoot) {
		t.Fatalf("PrivateTempDir wrote inside repository checkout %q: %q", repoRoot, dir)
	}
	info, err := os.Stat(dir)
	NoError(t, err)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected mode 0700, got %o", info.Mode().Perm())
	}
}
